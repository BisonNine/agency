package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/internal/mcpreg"
	"go.bison9.com/agency/report"
	"go.bison9.com/agency/web/backend/middleware"
)

// No test in this package may reach a real CLI: the shared registrar finds no
// binary at all. Tests that need CLIs give the handler a fake one.
func init() {
	defaultRegistrarOnce.Do(func() {
		defaultRegistrar = mcpreg.New(
			mcpreg.WithHome("/nonexistent"),
			mcpreg.WithEnv([]string{"PATH=/nonexistent"}),
			mcpreg.WithLookPath(func(string) (string, error) { return "", exec.ErrNotFound }),
			mcpreg.WithRunner(func(context.Context, string, []string, string, ...string) ([]byte, []byte, error) {
				return nil, nil, errors.New("no CLI may run in tests")
			}),
		)
	})
}

// fakeAgy is an in-process Antigravity CLI: its entry is url ("" for none).
// Only agy (and claude, which always fails) are "installed".
type fakeAgy struct {
	mu      sync.Mutex
	url     string
	fail    bool
	running atomic.Int32
	peak    atomic.Int32
	adds    atomic.Int32
}

func (f *fakeAgy) run(_ context.Context, _ string, _ []string, path string, args ...string) ([]byte, []byte, error) {
	n := f.running.Add(1)
	defer f.running.Add(-1)
	if p := f.peak.Load(); n > p {
		f.peak.CompareAndSwap(p, n)
	}
	time.Sleep(5 * time.Millisecond)
	if filepath.Base(path) != "agy" {
		return nil, []byte("token=SECRET-DETAIL"), errors.New("exit status 1")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.Join(args[:2], " ") {
	case "mcp list":
		if f.url == "" {
			return []byte("No MCP servers configured.\n"), nil, nil
		}
		return []byte("agency (http): " + f.url + "\n"), nil, nil
	case "mcp add":
		if f.fail {
			return nil, []byte("token=SECRET-DETAIL"), errors.New("exit status 2")
		}
		f.adds.Add(1)
		f.url = args[len(args)-1]
	}
	return nil, nil, nil
}

func (f *fakeAgy) set(url string) {
	f.mu.Lock()
	f.url = url
	f.mu.Unlock()
}

// cliRegisterHandler is a handler on a config with the MCP host on at
// 127.0.0.1:5999 (or off), with fake CLIs.
func cliRegisterHandler(t *testing.T, hostOn bool) (*Handler, *fakeAgy, *http.ServeMux) {
	t.Helper()
	path := setupTestEnv(t)
	h := NewHandler(path)
	if err := h.updateConfig(nil, func(cfg *config.Config) error {
		cfg.MCPHost.Enabled = hostOn
		cfg.MCPHost.AutoEnable = false
		cfg.MCPHost.Listen = "127.0.0.1:5999"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeAgy{}
	h.registrar = mcpreg.New(
		mcpreg.WithHome(t.TempDir()),
		mcpreg.WithEnv([]string{"PATH=/nonexistent"}),
		mcpreg.WithRunner(f.run),
		mcpreg.WithLookPath(func(name string) (string, error) {
			if name == "agy" || name == "claude" {
				return "/fake/" + name, nil
			}
			return "", exec.ErrNotFound
		}),
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, f, mux
}

const fakeURL = "http://127.0.0.1:5999/internal"

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	decodeInto(t, rec, &v)
	return v
}

func TestCLIRegistrations_List(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	f.set("http://127.0.0.1:1/internal")
	rec := serve(mux, http.MethodGet, "/api/system/clis/registration", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := decodeAs[cliRegistrations](t, rec)
	if got.URL != fakeURL || len(got.CLIs) != len(modelspec.CLIAgents) {
		t.Fatalf("%+v", got)
	}
	want := map[string]mcpreg.State{
		"claude-cli":      mcpreg.NotChecked,
		"codex-cli":       mcpreg.NotInstalled,
		"antigravity-cli": mcpreg.OtherAddress,
		"cursor-cli":      mcpreg.NotInstalled,
		"opencode-cli":    mcpreg.NotInstalled,
	}
	for _, st := range got.CLIs {
		if st.State != want[st.Protocol] {
			t.Errorf("%s: %+v", st.Protocol, st)
		}
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("command output in the answer: %s", rec.Body)
	}
}

func TestCLIRegistrations_HostOff(t *testing.T) {
	_, _, mux := cliRegisterHandler(t, false)
	got := decodeAs[cliRegistrations](t, serve(mux, http.MethodGet, "/api/system/clis/registration", ""))
	if got.URL != "" {
		t.Errorf("url %q", got.URL)
	}
	for _, st := range got.CLIs {
		if st.Protocol == "antigravity-cli" && st.State != mcpreg.HostOff {
			t.Errorf("%+v", st)
		}
	}
	rec := serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", `{}`)
	body := decodeAs[cliRegisterError](t, rec)
	if rec.Code != http.StatusConflict || body.Error != "Could not register Antigravity CLI: the tool server is off." || body.Status.State != mcpreg.HostOff {
		t.Errorf("%d %+v", rec.Code, body)
	}
}

func TestCLIRegister_Registers(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	// No body is the same as replace false.
	rec := serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", "")
	st := decodeAs[mcpreg.Status](t, rec)
	if rec.Code != http.StatusOK || st.State != mcpreg.Registered || st.Label != "Antigravity CLI" {
		t.Fatalf("%d %+v", rec.Code, st)
	}
	if f.url != fakeURL {
		t.Errorf("registered %q", f.url)
	}
}

// The server reads the state again: an entry pointing elsewhere is replaced
// only with replace, whatever the page showed.
func TestCLIRegister_ReplaceRequiredAndRechecked(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	got := decodeAs[cliRegistrations](t, serve(mux, http.MethodGet, "/api/system/clis/registration", ""))
	for _, st := range got.CLIs {
		if st.Protocol == "antigravity-cli" && st.State != mcpreg.NotRegistered {
			t.Fatalf("%+v", st)
		}
	}
	f.set("http://127.0.0.1:7000/internal") // changed behind the page's back
	rec := serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", `{"replace":false}`)
	body := decodeAs[cliRegisterError](t, rec)
	if rec.Code != http.StatusConflict || body.Status == nil || body.Status.Address != "http://127.0.0.1:7000/internal" ||
		body.Error != "Antigravity CLI is registered to a different address: http://127.0.0.1:7000/internal." {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f.adds.Load() != 0 {
		t.Fatal("replaced without replace")
	}
	rec = serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", `{"replace":true}`)
	if st := decodeAs[mcpreg.Status](t, rec); rec.Code != http.StatusOK || st.State != mcpreg.Registered {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestCLIRegister_Refusals(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	cases := []struct {
		path, body string
		code       int
		msg        string
	}{
		{"/api/system/clis/openai-chat/register", `{}`, http.StatusNotFound, "Unknown CLI."},
		{"/api/system/clis/codex-cli/register", `{}`, http.StatusConflict, "Codex CLI is not installed."},
		{"/api/system/clis/antigravity-cli/register", `{"replace":`, http.StatusBadRequest, "Invalid request."},
		{"/api/system/clis/antigravity-cli/register", `{"replace":"yes"}`, http.StatusBadRequest, "Invalid request."},
	}
	for _, c := range cases {
		rec := serve(mux, http.MethodPost, c.path, c.body)
		if rec.Code != c.code || decodeAs[cliRegisterError](t, rec).Error != c.msg {
			t.Errorf("%s %s: %d %s", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if f.adds.Load() != 0 {
		t.Error("a refused request registered")
	}
	if rec := serve(mux, http.MethodGet, "/api/system/clis/antigravity-cli/register", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET register: %d", rec.Code)
	}
}

// A failed command: one sentence naming the CLI; the detail only in the log.
func TestCLIRegister_Failure(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	f.fail = true
	rec := serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", `{}`)
	if rec.Code != http.StatusInternalServerError || decodeAs[cliRegisterError](t, rec).Error != "Could not register Antigravity CLI." {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("detail leaked: %s", rec.Body)
	}
}

func TestCLIRegister_Serialised(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if rec := serve(mux, http.MethodPost, "/api/system/clis/antigravity-cli/register", `{}`); rec.Code != http.StatusOK {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	if f.peak.Load() != 1 {
		t.Errorf("%d commands at once", f.peak.Load())
	}
}

func TestCLIRegister_AuthRequired(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	guarded := middleware.Auth(func() *middleware.AuthStore { return nil }, mux)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/system/clis/registration"},
		{http.MethodPost, "/api/system/clis/antigravity-cli/register"},
	} {
		if rec := serve(guarded, c.method, c.path, `{"replace":true}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if f.adds.Load() != 0 {
		t.Error("registered without a login")
	}
}

// Check Up has one tool server row per CLI an enabled model runs on, read
// through the same registrar.
func TestReportAssessment_ToolServerRows(t *testing.T) {
	h, f, mux := cliRegisterHandler(t, true)
	if err := h.updateConfig(nil, func(cfg *config.Config) error {
		cfg.Providers = append(cfg.Providers,
			modelspec.Provider{Name: "agy", Protocol: "antigravity-cli"},
			modelspec.Provider{Name: "cx", Protocol: "codex-cli"})
		cfg.Models = append(cfg.Models,
			modelspec.Model{ModelName: "agy", Model: "antigravity-cli", Provider: "agy", Enabled: true},
			modelspec.Model{ModelName: "cx", Model: "codex-cli", Provider: "cx", Enabled: false})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.set(fakeURL)
	got := decodeAs[report.Assessment](t, serve(mux, http.MethodGet, "/api/report/assessment", ""))
	var rows []report.AssessmentRow
	for _, r := range got.Assessment {
		if strings.HasPrefix(r.Item, "Tool server (") {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || rows[0].Item != "Tool server (Antigravity CLI)" || rows[0].Status != "Registered." || rows[0].Action {
		t.Fatalf("%+v", rows)
	}
	if rec := serve(mux, http.MethodGet, "/api/report/pdf", ""); rec.Code != http.StatusOK {
		t.Errorf("pdf: %d", rec.Code)
	}
}

// A registration that ran is audited: who, from where, which CLI, the
// address and whether it replaced; a refusal is not.
func TestCLIRegister_Audited(t *testing.T) {
	_, f, mux := cliRegisterHandler(t, true)
	initTestAudit(t)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/system/clis/antigravity-cli/register", strings.NewReader(body))
		req.RemoteAddr = "192.0.2.7:51000"
		req = req.WithContext(audit.WithActor(req.Context(), "admin"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	f.set("http://127.0.0.1:7000/internal")
	if rec := post(`{}`); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
	if rec := post(`{"replace":true}`); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	f.fail = true
	if rec := post(`{"replace":true}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("%d", rec.Code)
	}
	rows := cardRows(t, audit.KindCLIRegister)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	// Newest first.
	if rows[0].Outcome != audit.OutcomeError || rows[1].Outcome != audit.OutcomeOK {
		t.Errorf("outcomes %+v", rows)
	}
	r := rows[1]
	if r.Actor != "admin" || r.Sender != "192.0.2.7" || r.Summary != "antigravity-cli" ||
		r.Details != `{"replace":true,"url":"`+fakeURL+`"}` {
		t.Errorf("row %+v", r)
	}
}

// Without one of its own a handler uses the shared registrar, so
// registrations of one CLI are serialised across handlers.
func TestCLIRegistrar_Shared(t *testing.T) {
	a, b := NewHandler(""), NewHandler("")
	if a.cliRegistrar() == nil || a.cliRegistrar() != b.cliRegistrar() {
		t.Error("not shared")
	}
}

// With the MCP host off nothing is read; the rows say so from the config.
func TestReportAssessment_ToolServerRowsHostOff(t *testing.T) {
	h, _, mux := cliRegisterHandler(t, false)
	if err := h.updateConfig(nil, func(cfg *config.Config) error {
		cfg.Providers = append(cfg.Providers, modelspec.Provider{Name: "agy", Protocol: "antigravity-cli"})
		cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "agy", Model: "antigravity-cli", Provider: "agy", Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := decodeAs[report.Assessment](t, serve(mux, http.MethodGet, "/api/report/assessment", ""))
	found := false
	for _, r := range got.Assessment {
		if r.Item == "Tool server (Antigravity CLI)" {
			found = true
			if r.Status != "Tool server is off." || r.Link != "/mcp/config" || !r.Action {
				t.Errorf("%+v", r)
			}
		}
	}
	if !found {
		t.Error("no row")
	}
}
