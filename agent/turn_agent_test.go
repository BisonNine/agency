// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
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
	// The instance's close does not wait; the agent's own does, until the
	// close the instance started has finished.
	if err := built.Close(context.Background()); err != nil {
		t.Fatalf("agent Close: %v", err)
	}
	res := built.Run(context.Background(), "s", hook.UserText("hello")).Wait()
	if !errors.Is(res.Err, loop.ErrClosed) {
		t.Errorf("a run on a closed instance's SDK agent ended with %v, want %v", res.Err, loop.ErrClosed)
	}
}

// TestTurnAgentNotBuiltAfterClose checks that an instance made without the
// builder, once closed, builds no SDK agent for a turn (none that a Close
// would close): the turn ends with the error a closed instance's agent
// gives.
func TestTurnAgentNotBuiltAfterClose(t *testing.T) {
	al := newTestAgentLoop(t).al
	bob := &AgentInstance{ID: "bob"}
	if err := bob.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := al.runOnAgent(context.Background(), bob, processOptions{SessionKey: "agent:bob:main", UserMessage: "hello"}, &turnModel{}, nil, nil, false)
	if !errors.Is(err, loop.ErrClosed) {
		t.Errorf("a turn on a closed instance ended with %v, want %v", err, loop.ErrClosed)
	}
	bob.turnAgentMu.Lock()
	built := bob.turnAgent
	bob.turnAgentMu.Unlock()
	if built != nil {
		t.Error("a closed instance built an SDK agent")
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
	cm, mem, release := tl.al.getSessionContext(inst, opts.SessionKey)
	defer release()
	tm := tl.al.selectTurnModel(inst, opts.SessionKey)
	ctx, _ := tl.al.withHostTurn(context.Background(), inst, *opts, tm, cm, mem)
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

// TestTurnAgentWithoutSessionContextFails checks that a run given no
// session context ends with an error instead of building one of its own,
// and that a human agent's SDK agent is built with no model at all.
func TestTurnAgentWithoutSessionContextFails(t *testing.T) {
	tl := newTestAgentLoop(t)
	inst, ok := tl.al.GetRegistry().Get("main")
	if !ok {
		t.Fatal("agent main not built")
	}
	res := inst.turnAgent.Run(context.Background(), "agent:main:alice", hook.UserText("hello")).Wait()
	if !errors.Is(res.Err, errNoTurnSession) {
		t.Errorf("run without a session context ended with %v, want %v", res.Err, errNoTurnSession)
	}
	if h := inst.Sessions.GetHistory("agent:main:alice"); len(h) != 0 {
		t.Errorf("a run without a session context stored %d messages", len(h))
	}

	human, err := tl.al.newTurnAgent(&AgentInstance{ID: "bob", HumanModel: "bob-human"})
	if err != nil {
		t.Fatalf("human agent's SDK agent not built: %v", err)
	}
	if err := human.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}
