// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
)

// TestTurnAgentIsBuiltWithTheInstance checks that every instance the
// builder makes holds its SDK agent, under the instance's id and display
// name, and that closing the instance closes it: a run given afterwards ends
// at once with an error.
func TestTurnAgentIsBuiltWithTheInstance(t *testing.T) {
	tl := newTestAgentLoop(t)
	inst, ok := tl.al.GetRegistry().Get("main")
	if !ok {
		t.Fatal("agent main not built")
	}
	if inst.turnAgent == nil {
		t.Fatal("instance has no SDK agent")
	}
	if got := inst.turnAgent.ID(); got != inst.ID {
		t.Errorf("SDK agent id = %q, want %q", got, inst.ID)
	}
	if got := inst.turnAgent.Name(); got != inst.DisplayName() {
		t.Errorf("SDK agent name = %q, want %q", got, inst.DisplayName())
	}

	built, err := tl.al.newTurnAgent(inst)
	if err != nil {
		t.Fatalf("newTurnAgent: %v", err)
	}
	closing := &AgentInstance{ID: "bob", turnAgent: built}
	if err := closing.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res := built.Run(context.Background(), "s", hook.UserText("hello")).Wait()
	if res.Err == nil {
		t.Error("a run on a closed instance's SDK agent did not fail")
	}
}

// TestTurnAgentRunsATurnWithTheHostsSettings runs one turn on an instance's
// SDK agent: it answers through the instance's model choice and stores the
// exchange in the instance's session store, on the chat of the turn's
// options.
func TestTurnAgentRunsATurnWithTheHostsSettings(t *testing.T) {
	tl := newTestAgentLoop(t)
	inst, ok := tl.al.GetRegistry().Get("main")
	if !ok {
		t.Fatal("agent main not built")
	}
	opts := &processOptions{SessionKey: "agent:main:alice", Channel: "cli", ChatID: "direct", UserMessage: "hello"}
	ctx := withTurnOpts(context.Background(), opts)
	res := inst.turnAgent.Run(ctx, opts.SessionKey, hook.UserText(opts.UserMessage)).Wait()
	if res.Err != nil {
		t.Fatalf("turn failed: %v", res.Err)
	}
	if res.Reply != "Mock response" {
		t.Errorf("reply = %q, want the model's answer", res.Reply)
	}
	history := inst.Sessions.GetHistory(opts.SessionKey)
	stored := make([]string, 0, len(history))
	for _, m := range history {
		stored = append(stored, m.Role+":"+m.Content)
	}
	if got := strings.Join(stored, "|"); got != "user:hello|assistant:Mock response" {
		t.Errorf("stored = %q", got)
	}
}
