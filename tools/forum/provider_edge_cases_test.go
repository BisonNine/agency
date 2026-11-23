package forum

import (
	"context"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolspec"

	forumpkg "go.bison9.com/agency/forum"
	"go.bison9.com/agency/tools"
)

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != Suite || GlobalProvider.Description() != "Forums: set up and run structured discussions among agents" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}

// An agent without a workspace gets no forum tools.
func TestProvider_NoWorkspace(t *testing.T) {
	svc := forumpkg.New(forumpkg.Host{Messenger: nopHost{}, Agents: nopHost{}, Notifier: nopHost{}, Logger: nopHost{}})
	t.Cleanup(func() {
		if err := svc.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		SetService(nil)
	})
	SetService(svc)
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: scopeConfig(), AgentID: "alice", Host: tools.ToolDeps{}}); len(defs) != 0 {
		t.Errorf("no workspace: %d tools", len(defs))
	}
}

// The host answers only for its own agent, and resolves only what the
// agent's file tools may read.
func TestToolHost_OtherAgentAndUnreadable(t *testing.T) {
	ws := t.TempDir()
	cfg := scopeConfig()
	cfg.Agents.Defaults.RestrictToWorkspace = true
	h := &toolHost{cfg: cfg, agentID: "alice", workspace: ws}
	if err := h.ReadAllowed("bob", filepath.Join(ws, "files", "x.md")); err == nil || err.Error() != "agent bob is not agent alice" {
		t.Errorf("ReadAllowed for bob: %v", err)
	}
	if _, err := h.ResolveFile("alice", "../outside.md"); err == nil || err.Error() != "the agent may not read it or it does not exist" {
		t.Errorf("ResolveFile outside: %v", err)
	}
}
