// ClawEh
// License: MIT

package session

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/tools"
)

// session_clear is refused in an asked turn and works from a chat.
func TestSessionClear_RefusedInAskedTurn(t *testing.T) {
	cleared := 0
	deps := tools.ToolDeps{ClearFn: func(context.Context, string, string) error { cleared++; return nil }}
	var clearTool toolkit.Tool
	for _, tl := range tools.NamespacedProvider("session", GlobalProvider).Build(deps) {
		if tl.Name() == "session_clear" {
			clearTool = tl
		}
	}
	if clearTool == nil {
		t.Fatal("no session_clear")
	}
	run := func(channel string) *toolkit.Result {
		ctx := toolkit.WithSessionKey(toolkit.WithChat(context.Background(), channel, "x"), "agent:alice:main")
		return clearTool.Execute(ctx, map[string]any{})
	}
	if res := run(constants.AgentMessageChannel); !res.IsError || !strings.Contains(res.ForLLM, "answering another agent's message") || !toolkit.IsExpectedRefusal(res.Err) || cleared != 0 {
		t.Fatalf("asked turn: %+v cleared=%d, want an expected refusal", res, cleared)
	}
	if res := run("telegram"); res.IsError || cleared != 1 {
		t.Fatalf("chat turn: %+v cleared=%d, want the clear queued", res, cleared)
	}
}
