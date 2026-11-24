// Agency
// License: MIT

package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/channels/device"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// GET /api/agents/tools lists each configured MCP server once enabled, by
// name order, with the tool pattern that grants it.
func TestHandleListAgentTools_MCPServers(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	if err := h.updateConfig(nil, func(c *config.Config) error {
		c.Tools.MCP.Servers = map[string]config.MCPServerConfig{
			"zeta":  {Enabled: true, Command: "zeta-server"},
			"alpha": {Enabled: false, Command: "alpha-server"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := serve(mux, http.MethodGet, "/api/agents/tools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body agentToolCatalogResponse
	decodeInto(t, rec, &body)
	want := []agentMCPServer{
		{Name: "alpha", Pattern: tools.MCPServerPattern("alpha")},
		{Name: "zeta", Pattern: tools.MCPServerPattern("zeta")},
	}
	if len(body.MCPServers) != 2 || body.MCPServers[0] != want[0] || body.MCPServers[1] != want[1] {
		t.Fatalf("mcp_servers = %+v, want %+v", body.MCPServers, want)
	}
	if len(body.Tools) == 0 {
		t.Fatal("no built-in tools listed")
	}
}

// A failing reload after a task-model save is logged; the save stands.
func TestHandleUpdateTaskModels_ReloadFailure(t *testing.T) {
	configPath, _, _ := setupTaskModelsEnv(t)
	h := NewHandler(configPath)
	h.SetReloadTrigger(func() error { return errors.New("boom") })
	m := http.NewServeMux()
	h.RegisterRoutes(m)
	rec := serveTaskModels(m, http.MethodPut, `{"result_text_chars":2500,"classify":[{"model":"custom-default"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := loadSaved(t, configPath).TaskModels.ResultTextChars; got != 2500 {
		t.Fatalf("saved result_text_chars = %d, want 2500", got)
	}
}

// PUT /api/system/clis/{protocol} refuses a body that is not JSON.
func TestSetCLI_BadJSON(t *testing.T) {
	_, mux := providerMux(t)
	rec := serve(mux, http.MethodPut, "/api/system/clis/claude-cli", "{")
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "Invalid JSON: ") {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
}

// POST /api/webui/setup turns the WebUI channel on with a fresh token and an
// open allow list, saves it, and reports no change the second time.
func TestWebUISetup(t *testing.T) {
	p, mux := providerMux(t)
	rec := serve(mux, http.MethodPost, "/api/webui/setup", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"changed":true,"enabled":true}`+"\n" {
		t.Fatalf("first = %d %q", rec.Code, rec.Body.String())
	}
	w := loadSaved(t, p).Channels.WebUI
	if !w.Enabled || len(w.Token) != 32 || len(w.AllowFrom) != 1 || w.AllowFrom[0] != "*" {
		t.Fatalf("saved webui = enabled %v token %q allow_from %v", w.Enabled, w.Token, w.AllowFrom)
	}
	rec = serve(mux, http.MethodPost, "/api/webui/setup", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"changed":false,"enabled":true}`+"\n" {
		t.Fatalf("second = %d %q", rec.Code, rec.Body.String())
	}
	if again := loadSaved(t, p).Channels.WebUI; again.Token != w.Token {
		t.Fatal("second setup replaced the token")
	}
}

// Each enabled single channel counts once; telegram bots each count.
func TestCountChannels_Counts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channels.Telegram = []config.TelegramBotConfig{{ID: "a"}, {ID: "b"}}
	cfg.Channels.Device.Enabled = true
	cfg.Channels.WebUI.Enabled = true
	cfg.Channels.Discord.Enabled = false
	cfg.Channels.Slack.Enabled = false
	cfg.Channels.Matrix.Enabled = false
	cfg.Channels.SecMsg = nil
	if got := countChannels(cfg); got != 4 {
		t.Fatalf("countChannels = %d, want 4", got)
	}
}

// An enabled model whose provider is missing does not make the install usable.
func TestSetupStatus_ModelWithoutProvider(t *testing.T) {
	p := setupTestEnv(t)
	cfg := loadSaved(t, p)
	cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "orphan", Model: "m", Provider: "gone", Enabled: true})
	cfg.Models[0].Enabled = false
	if err := config.SaveConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	var got setupStatusResponse
	decodeInto(t, serve(mux, http.MethodGet, "/api/system/setup-status", ""), &got)
	if got.HasUsableModel || !got.NeedsSetup {
		t.Fatalf("setup status = %+v, want no usable model", got)
	}
}

// The alerts tail drops a partial first line when it starts mid-file, caps
// the line count, and reports a path it cannot read.
func TestGatewayAlerts_TailEdges(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	path := filepath.Join(t.TempDir(), "alerts.log")
	long := strings.Repeat("x", 2000)
	if err := os.WriteFile(path, []byte(long+"\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.SetAlertsPath(path)
	type alertsReply struct {
		Logs  []string `json:"logs"`
		Count int      `json:"count"`
		Error string   `json:"error"`
	}
	var out alertsReply
	decodeInto(t, serve(mux, http.MethodGet, "/api/gateway/alerts?lines=1", ""), &out)
	if len(out.Logs) != 1 || out.Logs[0] != "last" {
		t.Fatalf("lines=1 = %+v", out)
	}
	out = alertsReply{}
	decodeInto(t, serve(mux, http.MethodGet, "/api/gateway/alerts?lines=999999", ""), &out)
	if out.Count != 2 {
		t.Fatalf("capped = %+v", out)
	}
	h.SetAlertsPath(t.TempDir())
	out = alertsReply{}
	decodeInto(t, serve(mux, http.MethodGet, "/api/gateway/alerts", ""), &out)
	if out.Error == "" || out.Count != 0 {
		t.Fatalf("a folder = %+v, want an error", out)
	}
}

// The pairing response falls back to the default port and the loopback host
// when neither is set; an agent with no name is offered under its id.
func TestBuildPairResponse_Defaults(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channels.Device.Host = ""
	cfg.Channels.Device.Port = 0
	out := (&Handler{}).buildPairResponse(cfg, false)
	if out["listen_port"] != device.DefaultDevicePort || out["listen_host"] != "127.0.0.1" || out["listen_lan"] != false {
		t.Fatalf("listen = %v %v %v", out["listen_port"], out["listen_host"], out["listen_lan"])
	}
	cfg.Agents.List = []config.AgentConfig{{ID: "Bob"}, {ID: "alice", Name: "Alice"}}
	got := configuredAgents(cfg)
	if len(got) != 2 || got[0] != (agentOption{ID: "bob", Name: "bob"}) || got[1] != (agentOption{ID: "alice", Name: "Alice"}) {
		t.Fatalf("configuredAgents = %+v", got)
	}
}

// POST /api/devices/{id}/user refuses a body that is not JSON.
func TestDeviceUser_BadBody(t *testing.T) {
	_, mux, _ := deviceMux(t)
	rec := serve(mux, http.MethodPost, "/api/devices/dev1/user", "{")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"invalid body"}`+"\n" {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
}
