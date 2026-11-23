package shell

import (
	"context"
	"strings"
	"testing"

	sdkshell "go.bison9.com/agent/toolbox/shell"
	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/tools"
)

func TestProvider_MetadataAndEnumeration(t *testing.T) {
	p := provider{m: testMount(t)}
	if p.Namespace() != "shell" || p.Description() != "Shell commands and background jobs" {
		t.Errorf("metadata: %q %q", p.Namespace(), p.Description())
	}
	if ok, why := p.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	// Enumerated without real dependencies: shell_exec alone.
	defs := p.RegisterTools(toolspec.Deps{})
	if len(defs) != 1 || "shell_"+defs[0].Name != config.ShellExecTool {
		t.Errorf("enumeration: %d definitions", len(defs))
	}
}

// No tools are built for an agent without shell_exec, with an invalid
// allow pattern, without a workspace, or once the mount is closing.
func TestNoToolsBuilt(t *testing.T) {
	m := testMount(t)
	deps := func(agent *config.AgentConfig, ws string) tools.ToolDeps {
		return tools.ToolDeps{Cfg: config.DefaultConfig(), AgentCfg: agent, AgentID: agent.ID, Workspace: ws}
	}
	if defs := m.definitions(toolspec.Deps{Host: deps(&config.AgentConfig{ID: "alice"}, t.TempDir())}); defs != nil {
		t.Errorf("agent without shell_exec: %d tools", len(defs))
	}
	badAllow := deps(shellAgent("alice"), t.TempDir())
	badAllow.Cfg.Tools.Exec.CustomAllowPatterns = []string{"(["}
	if defs := m.definitions(toolspec.Deps{Host: badAllow}); defs != nil {
		t.Errorf("invalid allow pattern: %d tools", len(defs))
	}
	if defs := m.definitions(toolspec.Deps{Host: deps(shellAgent("alice"), "")}); defs != nil {
		t.Errorf("no workspace: %d tools", len(defs))
	}

	closing := newMount()
	if err := closing.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if defs := closing.definitions(toolspec.Deps{Host: deps(shellAgent("alice"), t.TempDir())}); defs != nil {
		t.Errorf("closing mount: %d tools", len(defs))
	}
}

// A custom deny pattern refuses what it matches, on top of the defaults.
func TestCustomDenyPattern(t *testing.T) {
	m := testMount(t)
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.CustomDenyPatterns = []string{`\bcurl\b`}
	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})
	if res := alice.run("shell_exec", map[string]any{"command": "curl https://example.com"}, nil); !strings.Contains(res.ForLLM, "Command blocked by safety guard") {
		t.Errorf("custom deny: %q", res.ForLLM)
	}
	if res := alice.run("shell_exec", map[string]any{"command": "echo ok"}, nil); res.IsError {
		t.Errorf("allowed command: %q", res.ForLLM)
	}
}

// A call that did not come through the built tools has no call info: its
// jobs belong to the calling agent and it has no shell_ctrl.
func TestCallWithoutInfo(t *testing.T) {
	call := &toolspec.ToolCall{AgentID: "bob"}
	if got := owner(call); got != "bob" {
		t.Errorf("owner = %q", got)
	}
	if ctrlAvailable(call) {
		t.Error("shell_ctrl without call info")
	}
	if _, ok := infoOf(nil); ok {
		t.Error("info of a nil call")
	}
	if callContext(call) == nil {
		t.Error("no context for a call without one")
	}
}

// Notices kept until the hooks are bound are capped.
func TestPendingNoticesCapped(t *testing.T) {
	m := newMount()
	for i := range maxPending + 3 {
		m.notice(sdkshell.Notice{Owner: "alice", JobID: "j" + string(rune('a'+i%26)), State: proc.StateLost, Text: "lost"})
	}
	if got := m.Pending(); got != maxPending {
		t.Errorf("pending = %d, want %d", got, maxPending)
	}
}

// An audit event of a kind the host does not record writes no row.
func TestAuditUnknownKind(t *testing.T) {
	if err := audit.Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := audit.Close(); err != nil {
			t.Errorf("audit Close: %v", err)
		}
	})
	m := newMount()
	m.audit(sdkshell.AuditEvent{Kind: sdkshell.AuditKind("unknown"), Owner: "alice", JobID: "j1"})
	m.audit(sdkshell.AuditEvent{Kind: sdkshell.AuditStop, Owner: "alice", JobID: "j2"})
	ctx := context.Background()
	store := audit.Default()
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Query(ctx, audit.Filter{Agent: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != audit.KindShellStop || !strings.Contains(rows[0].Details, `"job":"j2"`) {
		t.Errorf("rows = %+v", rows)
	}
}
