// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// catalogReloadConfig is a config for Alice on model alpha, in baseDir.
func catalogReloadConfig(baseDir, agentName string) *config.Config {
	return &config.Config{
		Agents: config.AgentsConfig{
			BaseDir:  baseDir,
			Defaults: config.AgentDefaults{Models: []string{"alpha"}, MaxTokens: 4096, MaxToolIterations: 10},
			List:     []config.AgentConfig{{ID: "alice", Name: agentName, Default: true}},
		},
		Providers: []config.Provider{{Name: "p", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}},
		Models:    []config.ModelConfig{{ModelName: "alpha", Model: "alpha-wire", Provider: "p", Enabled: true}},
	}
}

// A config save that does not touch models keeps the running catalogue, and
// with it the cooldowns; one that does hands the agents a new catalogue whose
// models start available.
func TestReload_CooldownsKeptUnlessModelsChange(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	model := &mockProvider{}
	al := mustNewAgentLoop(t, catalogReloadConfig(base, "Alice"), bus.NewMessageBus(), model, nil)
	client := &agentProviderClient{al: al, fallback: model}
	parkFor(t, al.catalog, "alpha", time.Minute, client)
	parked := func() bool {
		info, ok := al.catalog.Get("alpha")
		return ok && !info.Health.Available
	}

	// Renaming the agent changes nothing the catalogue is built from.
	if err := al.ReloadProviderAndConfig(context.Background(), model, catalogReloadConfig(base, "Alice Smith")); err != nil {
		t.Fatal(err)
	}
	if !parked() {
		t.Fatal("a save that does not touch models cleared the cooldown")
	}
	if a, _ := al.GetRegistry().Get("alice"); a.Name != "Alice Smith" {
		t.Fatalf("the reload did not apply: name %q", a.Name)
	}
	tm := al.selectTurnModel(al.GetRegistry().Default(), "s")
	if len(tm.candidates) != 1 || tm.candidates[0].Name != "alpha" {
		t.Fatalf("after the reload the agent's chain is %+v, want alpha", tm.candidates)
	}

	// A changed model setting is a new catalogue: cooldowns are cleared.
	changed := catalogReloadConfig(base, "Alice Smith")
	changed.Models[0].RequestTimeout = 90
	if err := al.ReloadProviderAndConfig(context.Background(), model, changed); err != nil {
		t.Fatal(err)
	}
	if parked() {
		t.Fatal("a save that changes models kept the cooldown")
	}
	if info, _ := al.catalog.Get("alpha"); info.Model.RequestTimeout != 90 {
		t.Fatalf("request timeout = %d, want the new 90", info.Model.RequestTimeout)
	}
	tm = al.selectTurnModel(al.GetRegistry().Default(), "s")
	if len(tm.candidates) != 1 || tm.candidates[0].Name != "alpha" {
		t.Fatalf("after adopting, the agent's chain is %+v, want alpha", tm.candidates)
	}

	// An agent created after the adoption gets its models in the running
	// catalogue, not in the spent one the reload built.
	cloneID, err := al.GetRegistry().CreateClone("alice")
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := al.GetRegistry().Get(cloneID)
	if tm := al.selectTurnModel(clone, "c"); len(tm.candidates) != 1 || tm.candidates[0].Name != "alpha" {
		t.Fatalf("a clone made after the reload has chain %+v, want alpha", tm.candidates)
	}
}

// The digest follows what the catalogue is built from, and nothing else.
func TestCatalogDigest(t *testing.T) {
	base := catalogReloadConfig("/x", "Alice")
	same := func(change func(*config.Config)) bool {
		c := catalogReloadConfig("/x", "Alice")
		change(c)
		return catalogDigest(c) == catalogDigest(base)
	}
	for name, tc := range map[string]struct {
		change func(*config.Config)
		same   bool
	}{
		"agent renamed":         {func(c *config.Config) { c.Agents.List[0].Name = "Bob" }, true},
		"channel setting":       {func(c *config.Config) { c.Agents.Defaults.MaxToolIterations = 3 }, true},
		"model setting":         {func(c *config.Config) { c.Models[0].ReasoningEffort = "high" }, false},
		"provider key":          {func(c *config.Config) { c.Providers[0].APIKey = "other" }, false},
		"agent models":          {func(c *config.Config) { c.Agents.List[0].Models = []string{"alpha"} }, false},
		"cooldown policy":       {func(c *config.Config) { c.Cooldown.RateLimitMinutes = 3 }, false},
		"log content":           {func(c *config.Config) { c.Logging.LogMessageContent = true }, false},
		"agents' request limit": {func(c *config.Config) { c.Agents.Defaults.RequestTimeout = 30 }, false},
		"default context":       {func(c *config.Config) { c.Agents.Defaults.ContextWindow = 1000 }, false},
	} {
		if got := same(tc.change); got != tc.same {
			t.Errorf("%s: digest unchanged = %v, want %v", name, got, tc.same)
		}
	}
}

// deadlineProvider holds the first model until the call's context ends.
type deadlineProvider struct{ replayProvider }

func (p *deadlineProvider) Chat(ctx context.Context, id providers.Identity, m []providers.Message, tl []providers.ToolDefinition, model string, o map[string]any) (*providers.LLMResponse, error) {
	if model == "gpt-4" {
		p.mu.Lock()
		p.called = append(p.called, model)
		p.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.replayProvider.Chat(ctx, id, m, tl, model, o)
}

// When the turn's deadline passes during a model call, the chain stops: the
// next model is not called on the dead context, no fallback is announced or
// logged, and the turn's reply is the time-limit message.
func TestCallModel_TurnDeadlineStopsTheChain(t *testing.T) {
	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	p := &deadlineProvider{}
	turn, d := chainTurn(t, p, nil, replayGPT, replayClaude)
	var notices int
	turn.notifier = func([]providers.FallbackAttempt, providers.FallbackCandidate) { notices++ }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := turn.requestModel(ctx, d)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if got := p.calls(); !slices.Equal(got, []string{"gpt-4"}) {
		t.Fatalf("calls = %v, want only the model the deadline cut short", got)
	}
	if notices != 0 {
		t.Fatalf("%d fallback notices, want none", notices)
	}
	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish)
	if got := renderTurnError(ctx, 15*time.Minute, err); !strings.HasPrefix(got, "⚠️ This turn ran past the 15m time limit") {
		t.Fatalf("reply = %q, want the time-limit message", got)
	}
}

// entryNames are the names of a set's list.
func entryNames(entries []models.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// Each agent's set holds its models by purpose, in list order: chat, worker
// (only when configured; else the Worker → Chat rule), the summarization
// chain for summaries and memory (primary last, once), and every task-model
// use with its entry as settings.
func TestAssignAgentModels_Purposes(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := catalogReloadConfig(t.TempDir(), "Alice")
	for _, n := range []string{"beta", "gamma", "eyes", "pics"} {
		cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: n, Model: n + "-wire", Provider: "p", Enabled: true})
	}
	cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: "nope", Model: "nope-wire", Provider: "p"}) // disabled
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{
		ID: "bob", Name: "Bob", Models: []string{"beta", "alpha"},
		Subagents:           &config.SubagentsConfig{Models: []string{"gamma", "nope"}},
		SummarizationModels: []string{"gamma", "alpha"},
	})
	cfg.Summarization.Models = []string{"beta", "gamma"}
	cfg.TaskModels.VisionModels = []config.TaskModelRef{{Model: "eyes"}}
	cfg.TaskModels.ImageModels = []config.ImageTaskModel{{Model: "pics", ImageAPI: config.ImageAPIImages}}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)

	bob := al.catalog.Set("bob")
	for _, tc := range []struct {
		purpose models.Purpose
		want    []string
	}{
		{models.Chat, []string{"beta", "alpha"}},
		{models.Worker, []string{"gamma"}},
		{models.Summarize, []string{"gamma", "alpha", "beta"}},
		{models.Memory, []string{"gamma", "alpha", "beta"}},
		{models.Purpose("vision"), []string{"eyes"}},
		{models.Purpose("image"), []string{"pics"}},
		{models.Purpose("tts"), []string{}},
	} {
		if got := entryNames(bob.List(tc.purpose)); !slices.Equal(got, tc.want) {
			t.Errorf("bob %s = %v, want %v", tc.purpose, got, tc.want)
		}
	}
	if e := bob.List(models.Purpose("image")); len(e) != 1 || !imageAPIIs(e[0].Settings, config.ImageAPIImages) {
		t.Errorf("image entry settings = %+v, want the entry's image_api", e)
	}
	alice := al.catalog.Set("alice")
	if got := entryNames(alice.List(models.Worker)); len(got) != 0 {
		t.Errorf("alice has worker models %v, want none", got)
	}
	if got := alice.Begin(models.Worker).Candidates(); len(got) != 1 || got[0].Name != "alpha" {
		t.Errorf("alice's workers run on %+v, want her chat model", got)
	}
	if got := entryNames(alice.List(models.Summarize)); !slices.Equal(got, []string{"beta", "gamma", "alpha"}) {
		t.Errorf("alice summarize = %v, want the global list then her primary", got)
	}
}

// Deleting a temporary agent's session drops what the catalogue holds for it.
func TestForgetSessionCaches_ForgetsSelection(t *testing.T) {
	al, agent := fourModelLoop(t)
	if err := al.setActiveModelIndex(agent, "s", 2); err != nil {
		t.Fatal(err)
	}
	// Drop the stored choice too, so only memory could still hold it.
	if store, ok := agent.Sessions.(compactionStateStore); ok {
		st, err := store.GetCompactionState("s")
		if err != nil {
			t.Fatal(err)
		}
		st.ActiveModelIndex = 0
		if err := store.SetCompactionState("s", st); err != nil {
			t.Fatal(err)
		}
	}
	al.forgetSessionCaches(agent, "s")
	if got := al.getActiveModelIndex(agent, "s"); got != 0 {
		t.Fatalf("active index after forget = %d, want 0", got)
	}
}

// imageAPIIs reports whether settings is a task-model entry with that
// image_api.
func imageAPIIs(settings any, api string) bool {
	e, ok := settings.(config.TaskModelEntry)
	return ok && e.ImageAPI == api
}
