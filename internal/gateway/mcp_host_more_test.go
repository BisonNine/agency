// ClawEh
// License: MIT

package gateway

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/servicetoken"
)

// mcpHostLoop is an agent loop for agent "alice" with the MCP host enabled on
// a kernel-picked port, and its alerts recorded.
func mcpHostLoop(t *testing.T) (*config.Config, *agent.AgentLoop, *alertRecorder) {
	t.Helper()
	t.Setenv(global.EnvVarHome, t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Agents.BaseDir = t.TempDir()
	cfg.Agents.Defaults.Models = []string{"test-model"}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}}
	cfg.MCPHost.Enabled = true
	cfg.MCPHost.Listen = "127.0.0.1:0"
	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	t.Cleanup(func() { al.Close(context.Background()) })
	return cfg, al, rec
}

func startTestMCPHost(t *testing.T, cfg *config.Config, al *agent.AgentLoop) *gatewayServices {
	t.Helper()
	services := &gatewayServices{fatal: newFatalNotifier(nil)}
	if err := startMCPServer(cfg, al, bus.NewMessageBus(), services); err != nil {
		t.Fatalf("startMCPServer: %v", err)
	}
	t.Cleanup(func() {
		if err := services.MCPServer.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return services
}

// TestStartMCPServer_TestToken: CLAW_MCP_TEST_TOKEN is a session token of the
// default agent.
func TestStartMCPServer_TestToken(t *testing.T) {
	cfg, al, _ := mcpHostLoop(t)
	t.Setenv("CLAW_MCP_TEST_TOKEN", "test-token-alice")
	services := startTestMCPHost(t, cfg, al)
	if _, ok := services.SessionTokens.Resolve("test-token-alice"); !ok {
		t.Fatal("the test token does not resolve")
	}
}

// TestSyncServiceTokens_BadFileAndUnknownAgent: an unreadable token file
// alerts and keeps the loaded set; a token for an agent that does not exist
// is skipped; nothing happens without a server.
func TestSyncServiceTokens_BadFileAndUnknownAgent(t *testing.T) {
	cfg, al, rec := mcpHostLoop(t)
	services := startTestMCPHost(t, cfg, al)
	path := servicetoken.Path(cfg.DataDir())

	if err := servicetoken.Save(path, map[string]string{"alice": "SVC-alice", "nobody": "SVC-nobody"}); err != nil {
		t.Fatal(err)
	}
	syncServiceTokensFromDisk(cfg, al, services.MCPServer)
	if _, ok := services.SessionTokens.Resolve("SVC-alice"); !ok {
		t.Error("alice's service token not loaded")
	}
	if _, ok := services.SessionTokens.Resolve("SVC-nobody"); ok {
		t.Error("a token for an unknown agent was loaded")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncServiceTokensFromDisk(cfg, al, services.MCPServer)
	if !rec.has("Service tokens not loaded") {
		t.Errorf("alerts = %v", rec.titles())
	}
	if _, ok := services.SessionTokens.Resolve("SVC-alice"); !ok {
		t.Error("a bad file dropped the loaded tokens")
	}

	syncServiceTokensFromDisk(cfg, al, nil) // no server: nothing to do
	syncServiceTokensFromDisk(nil, al, services.MCPServer)
}

// TestTempAgentTools_ConfigAgent: a config agent is not served through the
// temporary-agent lookup.
func TestTempAgentTools_ConfigAgent(t *testing.T) {
	_, al, _ := mcpHostLoop(t)
	lookup := tempAgentTools(al)
	for _, id := range []string{"alice", "nobody"} {
		if _, ok := lookup(id); ok {
			t.Errorf("lookup(%q) found a temporary agent", id)
		}
	}
}

// TestStartMCPServer_BadListen: an MCP host that cannot listen fails the
// start.
func TestStartMCPServer_BadListen(t *testing.T) {
	cfg, al, _ := mcpHostLoop(t)
	cfg.MCPHost.Listen = "127.0.0.1:not-a-port"
	services := &gatewayServices{fatal: newFatalNotifier(nil)}
	err := startMCPServer(cfg, al, bus.NewMessageBus(), services)
	if err == nil || !strings.Contains(err.Error(), "MCP server") {
		t.Fatalf("err = %v", err)
	}
}
