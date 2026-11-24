// Agency
// License: MIT

package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
)

// logLine is one captured log event: its level, message and every field.
type logLine struct {
	Level   string
	Message string
	Fields  map[string]any
}

// captureLog records what the package logs while the test runs.
func captureLog(t *testing.T) func() []logLine {
	t.Helper()
	var buf bytes.Buffer
	t.Cleanup(logger.RedirectForTest(&buf))
	return func() []logLine {
		var out []logLine
		for raw := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			if raw == "" {
				continue
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(raw), &fields); err != nil {
				t.Fatalf("log line %q: %v", raw, err)
			}
			level := fmt.Sprint(fields["level"])
			msg := fmt.Sprint(fields["message"])
			out = append(out, logLine{Level: level, Message: msg, Fields: fields})
		}
		return out
	}
}

// messages are the messages of lines.
func messages(lines []logLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Message)
	}
	return out
}

// openAIProvider is an HTTP provider whose clients are never called.
func openAIProvider(name string) modelspec.Provider {
	return modelspec.Provider{Name: name, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}
}

// A model the catalogue gets has the workspace and request timeout the
// dispatcher would fill in; a disabled model is left out, and a model's own
// settings stand.
func TestCatalogModels_FillsDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.RequestTimeout = 45
	cfg.Models = []modelspec.Model{
		{ModelName: "plain", Model: "m", Provider: "p", Enabled: true},
		{ModelName: "own", Model: "m", Provider: "p", Enabled: true, Workspace: "/srv/alice", RequestTimeout: 7},
		{ModelName: "off", Model: "m", Provider: "p"},
	}
	got := catalogModels(cfg)
	if len(got) != 2 || got[0].ModelName != "plain" || got[1].ModelName != "own" {
		t.Fatalf("models = %+v, want plain and own", got)
	}
	if want := filepath.Join(home, global.CLIDir); got[0].Workspace != want || got[0].RequestTimeout != 45 {
		t.Errorf("plain = workspace %q timeout %d, want %q and 45", got[0].Workspace, got[0].RequestTimeout, want)
	}
	if got[1].Workspace != "/srv/alice" || got[1].RequestTimeout != 7 {
		t.Errorf("own = workspace %q timeout %d, want its own", got[1].Workspace, got[1].RequestTimeout)
	}
	if cfg.Models[0].Workspace != "" {
		t.Error("catalogModels changed the configuration")
	}

	// Without a data directory or a default timeout nothing is filled in.
	bare := &config.Config{Models: cfg.Models[:1]}
	if m := catalogModels(bare)[0]; m.Workspace != "" || m.RequestTimeout != 0 {
		t.Errorf("bare config filled in workspace %q timeout %d", m.Workspace, m.RequestTimeout)
	}
}

// A provider or model the catalogue refuses is left out and logged, the rest
// is built; a quiet builder leaves them out without a word.
func TestBuild_LeavesOutAndLogsRefused(t *testing.T) {
	cfg := &config.Config{
		Providers: []modelspec.Provider{openAIProvider("good"), {Name: "bad", Protocol: "no-such-protocol"}},
		Models: []modelspec.Model{
			{ModelName: "ok", Model: "m", Provider: "good", Enabled: true},
			{ModelName: "orphan", Model: "m", Provider: "bad", Enabled: true},
		},
	}
	logs := captureLog(t)
	c := NewCatalogBuilder(nil, nil).Build(cfg)
	if _, ok := c.Get("ok"); !ok {
		t.Error("good model missing")
	}
	if _, ok := c.Get("orphan"); ok {
		t.Error("a model on a refused provider was added")
	}
	if e := ModelSet(c, " ok ").List(models.Chat); len(e) != 1 || e[0].Name != "ok" {
		t.Errorf("single-model set of ok = %+v", e)
	}
	lines := logs()
	want := []string{"provider left out of the model catalogue", "model left out of the model catalogue"}
	if got := messages(lines); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("logged %q, want %q", got, want)
	}
	if lines[0].Level != "warn" || lines[0].Fields["provider"] != "bad" || lines[1].Fields["model"] != "orphan" {
		t.Errorf("log lines = %+v", lines)
	}

	quiet := captureLog(t)
	newQuietCatalogBuilder().Build(cfg)
	if got := quiet(); len(got) != 0 {
		t.Errorf("quiet build logged %q", messages(got))
	}
}

// memStore is a SelectionStore in memory.
type memStore struct {
	mu    sync.Mutex
	saved map[string]string
}

func (s *memStore) LoadSelection(set string, p models.Purpose, scope models.Scope) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[set+"/"+string(p)+"/"+string(scope)], nil
}

func (s *memStore) SaveSelection(set string, p models.Purpose, scope models.Scope, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved[set+"/"+string(p)+"/"+string(scope)] = name
	return nil
}

// Catalogues built after SetSelectionStore persist their selections there,
// and report to the builder's alerts.
func TestBuild_SelectionStoreAndAlerts(t *testing.T) {
	alerts := NewModelAlerts()
	b := NewCatalogBuilder(alerts, nil)
	if b.Alerts() != alerts {
		t.Fatal("Alerts() is not the observer the builder was given")
	}
	if NewCatalogBuilder(nil, nil).Alerts() == nil {
		t.Fatal("a builder without alerts has no observer")
	}
	st := &memStore{saved: map[string]string{}}
	b.SetSelectionStore(st)
	cfg := &config.Config{
		Providers: []modelspec.Provider{openAIProvider("p")},
		Models:    []modelspec.Model{{ModelName: "alice-model", Model: "m", Provider: "p", Enabled: true}},
	}
	c := b.Build(cfg)
	if err := ModelSet(c, "alice-model").Select(models.Chat, "alice-model"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := st.saved[modelSetID("alice-model")+"/chat/"]; got != "alice-model" {
		t.Errorf("store holds %v, want the selection of alice-model", st.saved)
	}
}

// The digest changes with what the catalogue depends on, and only with it.
func TestDigest(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Providers: []modelspec.Provider{openAIProvider("p")},
			Models: []modelspec.Model{
				{ModelName: "a", Model: "m", Provider: "p", Enabled: true, ResponseLogFile: filepath.Join(t.TempDir(), "a.log")},
				{ModelName: "off", Model: "m", Provider: "p"},
			},
		}
	}
	cfg := base()
	d := Digest(cfg)
	if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not a hex SHA-256", d)
	}
	if Digest(cfg) != d {
		t.Error("the same configuration digests differently")
	}
	same := *cfg
	same.Models = append([]modelspec.Model(nil), cfg.Models...)
	same.Models[1].Model = "other" // a disabled model is not in the catalogue
	if Digest(&same) != d {
		t.Error("a disabled model's settings changed the digest")
	}
	for name, change := range map[string]func(*config.Config){
		"provider":    func(c *config.Config) { c.Providers[0].APIKey = "k2" },
		"model":       func(c *config.Config) { c.Models[0].Model = "m2" },
		"enabled":     func(c *config.Config) { c.Models[1].Enabled = true },
		"cooldown":    func(c *config.Config) { c.Cooldown.RateLimitMinutes++ },
		"log content": func(c *config.Config) { c.Logging.LogMessageContent = true },
		"max tokens":  func(c *config.Config) { c.Agents.Defaults.MaxTokens = 1000 },
	} {
		c := *cfg
		c.Providers = append([]modelspec.Provider(nil), cfg.Providers...)
		c.Models = append([]modelspec.Model(nil), cfg.Models...)
		change(&c)
		if Digest(&c) == d {
			t.Errorf("changing the %s left the digest unchanged", name)
		}
	}
}

// LogBypassEnabled names each CLI provider running with its bypass flag, and
// nothing else.
func TestLogBypassEnabled(t *testing.T) {
	logs := captureLog(t)
	LogBypassEnabled(nil)
	LogBypassEnabled(&config.Config{Providers: []modelspec.Provider{
		{Name: "Alice CLI", Protocol: "claude-cli", BypassRestrictions: true},
		{Name: "Bob CLI", Protocol: "codex-cli"},
		{Name: "HTTP", Protocol: "openai-chat", BypassRestrictions: true},
	}})
	lines := logs()
	if len(lines) != 1 {
		t.Fatalf("logged %q, want one line", messages(lines))
	}
	l := lines[0]
	if l.Level != "info" || l.Fields["provider"] != "Alice CLI" || l.Fields["protocol"] != "claude-cli" ||
		l.Message != "Allow CLI to bypass restrictions is on: the CLI can run commands and edit files anywhere the service user can, without asking" {
		t.Errorf("log line = %+v", l)
	}
}

// The declined-tools error lists the refused tools when the CLI named them.
func TestDeclinedMessage_ListsDeniedTools(t *testing.T) {
	got := declinedMessage(models.Declined{Label: "Claude CLI", Denied: []string{"Bash", "Edit"}})
	const want = "The Claude CLI declined to use tools (Bash, Edit). Tick *Allow CLI to bypass restrictions* for this CLI on the Providers page, or allow the tools in the CLI's own settings."
	if got != want {
		t.Errorf("message = %q\nwant %q", got, want)
	}
}

// A bypass flag in a CLI model's extra_args, on a provider that does not
// allow it, is dropped and logged when the client is built.
func TestBuild_LogsIgnoredBypassFlag(t *testing.T) {
	cfg := &config.Config{
		Providers: []modelspec.Provider{{Name: "Claude CLI", Protocol: "claude-cli", Command: "/bin/false"}},
		Models: []modelspec.Model{{
			ModelName: "c", Model: "claude-cli", Provider: "Claude CLI", Enabled: true, Workspace: t.TempDir(),
			ExtraArgs: []string{"--dangerously-skip-permissions"},
		}},
	}
	logs := captureLog(t)
	_, release, err := NewCatalogBuilder(nil, nil).Build(cfg).Client("c")
	if err != nil {
		t.Fatal(err)
	}
	release()
	var found bool
	for _, l := range logs() {
		if strings.HasPrefix(l.Message, "ignoring permission-bypass flag in extra_args") {
			found = l.Fields["flags"] == "--dangerously-skip-permissions" && l.Fields["protocol"] == "claude-cli"
		}
	}
	if !found {
		t.Error("the ignored bypass flag was not logged")
	}
}

// The default-model check names each reason the default model cannot be
// called, and passes a model that can be.
func TestCheckDefaultModel_Problems(t *testing.T) {
	good := openAIProvider("p")
	for _, tc := range []struct {
		name    string
		models  []modelspec.Model
		def     string
		wantErr string
	}{
		{"callable", []modelspec.Model{{ModelName: "a", Model: "m", Provider: "p", Enabled: true}}, "a", ""},
		{"unknown default", []modelspec.Model{{ModelName: "a", Model: "m", Provider: "p", Enabled: true}}, "b", `model "b" not found in models: `},
		{"provider missing", []modelspec.Model{{ModelName: "a", Model: "m", Provider: "gone", Enabled: true}}, "a", `model "a": `},
		{"not in catalogue", []modelspec.Model{{ModelName: "a", Provider: "p", Enabled: true}}, "a", `failed to create provider for model "a": `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Providers: []modelspec.Provider{good}, Models: tc.models}
			cfg.Agents.Defaults.Models = []string{tc.def}
			err := CheckDefaultModel(context.Background(), cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want prefix %q", err, tc.wantErr)
			}
		})
	}
}

// clientProblem reads each kind of client error the way operators have
// always read it, and passes any other error through.
func TestClientProblem(t *testing.T) {
	plain := errors.New("boom")
	cause := errors.New("dial refused")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not a client error", plain, "boom"},
		{"unknown protocol", &models.ClientError{Provider: "P", Protocol: "x", Err: models.ErrNoClient}, `provider "P": unknown protocol "x"`},
		{"task only", &models.ClientError{Provider: "P", Protocol: modelspec.GeminiProtocol, Err: models.ErrNoClient}, `provider "P": protocol "gemini" serves task models only (speech), not chat`},
		{"model required", &models.ClientError{Provider: "P", Err: modelspec.ErrModelRequired}, modelspec.ErrModelRequired.Error()},
		{"other", &models.ClientError{Provider: "P", Err: cause}, `provider "P": dial refused`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientProblem(tc.err); got.Error() != tc.want {
				t.Errorf("clientProblem = %q, want %q", got, tc.want)
			}
		})
	}
	if got := clientProblem(&models.ClientError{Provider: "P", Err: cause}); !errors.Is(got, cause) {
		t.Error("the cause is not wrapped")
	}
}

// A fallback and an exhausted chain are reported by the turn, not alerted.
func TestModelAlerts_FallbackAndExhaustionDoNotAlert(t *testing.T) {
	r := newAlertRig(t)
	set := r.cat.Set("alice")
	for i, p := range []string{"openai", "openrouter"} {
		if err := set.Assign(models.Chat, "m@"+p, i); err != nil {
			t.Fatal(err)
		}
	}
	id := accounting.Identity{Agent: "alice", Name: "Alice", Project: "default"}
	r.client.Script("m", modelstest.Status(500, 0), modelstest.Reply("ok"))
	if _, err := set.Begin(models.Chat).Chat(context.Background(), id, models.Request{}); err != nil {
		t.Fatalf("fallback call: %v", err)
	}
	r.client.Script("m", modelstest.Status(500, 0), modelstest.Status(500, 0))
	r.clk.Advance(24 * time.Hour)
	if _, err := set.Begin(models.Chat).Chat(context.Background(), id, models.Request{}); err == nil {
		t.Fatal("exhausted chain answered")
	}
	if got := r.rec.got(); len(got) != 0 {
		t.Errorf("alerts sent: %+v", got)
	}
}

// A conversation with no user message relays nothing.
func TestLatestUserMessage_None(t *testing.T) {
	if got := latestUserMessage([]spawnllm.Message{{Role: "system", Content: "s"}, {Role: "assistant", Content: "a"}}); got != "" {
		t.Errorf("latestUserMessage = %q, want empty", got)
	}
}
