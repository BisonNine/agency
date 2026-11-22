// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/tools/shell"
)

// shellRegistry is a registry holding the shell tools built for an agent
// granted shell_exec, over workspace.
func shellRegistry(t *testing.T, agentID, workspace string) *tools.ToolRegistry {
	t.Helper()
	agent := &config.AgentConfig{ID: agentID, Tools: []string{config.ShellExecTool}}
	built := tools.NamespacedProvider("shell", shell.GlobalProvider).Build(tools.ToolDeps{
		Cfg: config.DefaultConfig(), AgentCfg: agent, AgentID: agentID, Workspace: workspace,
	})
	if len(built) != 2 {
		t.Fatalf("built %d shell tools, want 2", len(built))
	}
	t.Cleanup(func() { shell.Host.StopAll(agentID) })
	return newRegistryWith(built...)
}

// TestDispatch_ShellFollowsTheTokensAgent: a shell_exec call over MCP runs
// when the token's agent has shell_exec, and is refused like any other tool
// the agent does not have when it does not, for a session token and a
// service token alike. The chat plays no part.
func TestDispatch_ShellFollowsTheTokensAgent(t *testing.T) {
	alice := shellRegistry(t, "alice", t.TempDir())
	bob := newRegistryWith() // Bob's tool permissions leave out shell_exec
	resolve := resolverFor(map[string]*tools.ToolRegistry{"alice": alice, "bob": bob})

	st := newSessionTokenStore()
	aliceTok := st.Issue("alice", "agent:alice:main", "/ws")
	bobTok := st.Issue("bob", "agent:bob:main", "/ws")
	aliceSvc := "SST" + strings.Repeat("ab", 32)
	bobSvc := "SST" + strings.Repeat("cd", 32)
	st.RegisterService(aliceSvc, "alice", "/ws")
	st.RegisterService(bobSvc, "bob", "/ws")

	bobRefusal := tools.NotEnabledMessage("shell_exec")
	for _, tc := range []struct {
		name, token, want string
		wantErr           bool
	}{
		{"alice session token", aliceTok, "shell-ok", false},
		{"alice service token", aliceSvc, "shell-ok", false},
		{"bob session token", bobTok, bobRefusal, true},
		{"bob service token", bobSvc, bobRefusal, true},
	} {
		out, isErr := dispatchToolCall(context.Background(), "shell_exec",
			map[string]any{"session_token": tc.token, "command": "echo shell-ok"},
			st, resolve, nil, nil, nil, nil)
		if isErr != tc.wantErr || !strings.Contains(out, tc.want) {
			t.Errorf("%s: (%q, isErr=%v), want %q isErr=%v", tc.name, out, isErr, tc.want, tc.wantErr)
		}
		if tc.wantErr && out != tc.want {
			t.Errorf("%s: refusal %q, want exactly %q", tc.name, out, tc.want)
		}
	}
}

// TestDispatch_ShellCtrlWaitEndsAtTheCallDeadline: shell_ctrl wait over MCP
// returns before the MCP call's deadline, however long it was asked to
// wait, and the job keeps running.
func TestDispatch_ShellCtrlWaitEndsAtTheCallDeadline(t *testing.T) {
	alice := shellRegistry(t, "alice", t.TempDir())
	resolve := resolverFor(map[string]*tools.ToolRegistry{"alice": alice})
	st := newSessionTokenStore()
	tok := st.Issue("alice", "agent:alice:main", "/ws")

	out, isErr := dispatchToolCall(context.Background(), "shell_exec",
		map[string]any{"session_token": tok, "command": "sleep 30", "background": true},
		st, resolve, nil, nil, nil, nil)
	if isErr {
		t.Fatalf("background start failed: %s", out)
	}
	id := regexp.MustCompile(`job (j[0-9a-f]{8})`).FindStringSubmatch(out)
	if id == nil {
		t.Fatalf("no job id in %q", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	out, isErr = dispatchToolCall(ctx, "shell_ctrl",
		map[string]any{"session_token": tok, "action": "wait", "id": id[1], "seconds": 120},
		st, resolve, nil, nil, nil, nil)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("wait took %v, past the call's 3s deadline", took)
	}
	if isErr || !strings.Contains(out, "still running") {
		t.Errorf("wait = (%q, isErr=%v), want the job still running", out, isErr)
	}
	if n := shell.Host.Running("alice"); n != 1 {
		t.Errorf("alice runs %d jobs after the wait, want 1", n)
	}
}
