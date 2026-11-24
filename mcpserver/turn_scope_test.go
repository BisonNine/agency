// Agency
// License: MIT

package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/tools"
)

// scopeTool records the ask chain, task and inherited billing code it ran
// with; set, when given, is the code it changes the run's to.
type scopeTool struct {
	chain   []string
	task    string
	billing string
	set     string
}

func (s *scopeTool) Name() string               { return "probe" }
func (s *scopeTool) Description() string        { return "records turn scope" }
func (s *scopeTool) Parameters() map[string]any { return map[string]any{} }
func (s *scopeTool) Execute(ctx context.Context, _ map[string]any) *toolkit.Result {
	s.chain = tools.AskChain(ctx)
	s.task = hook.TaskID(ctx)
	s.billing, _ = tools.Billing(ctx)
	if b := tools.InheritedBillingOf(ctx); b != nil && s.set != "" {
		b.Set(s.set)
	}
	return &toolkit.Result{ForLLM: "ok", ForUser: "for the chat"}
}

func dispatchScope(t *testing.T, st *SessionTokenStore, tok string, msgBus *bus.MessageBus) *scopeTool {
	t.Helper()
	return dispatchScopeTool(t, st, tok, msgBus, &scopeTool{})
}

func dispatchScopeTool(t *testing.T, st *SessionTokenStore, tok string, msgBus *bus.MessageBus, tool *scopeTool) *scopeTool {
	t.Helper()
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(tool)}
	if out, isErr := dispatchToolCall(context.Background(), "probe",
		map[string]any{"session_token": tok}, st, resolverFor(regs), nil, nil, msgBus, nil); isErr {
		t.Fatalf("dispatch failed: %s", out)
	}
	return tool
}

// A CLI agent's tool calls carry the ask chain and task of the turn holding
// the token, as in-process calls do (so an agent waiting in the
// exchange cannot be asked over MCP either); a later turn replaces them, and
// a token rotation keeps them.
func TestDispatch_CarriesTurnScope(t *testing.T) {
	st := newSessionTokenStore()
	st.Issue("alice", "agent:alice:main", "/ws")

	st.SetTurnScope("agent:alice:main", []string{"bob", "alice"}, "task-1", nil)
	tok := st.Issue("alice", "agent:alice:main", "/ws") // rotation keeps the scope
	got := dispatchScope(t, st, tok, nil)
	if !slices.Equal(got.chain, []string{"bob", "alice"}) || got.task != "task-1" {
		t.Fatalf("scope = %v in %q, want [bob alice] in task-1", got.chain, got.task)
	}

	st.SetTurnScope("agent:alice:main", []string{"alice"}, "", nil)
	got = dispatchScope(t, st, tok, nil)
	if !slices.Equal(got.chain, []string{"alice"}) || got.task != "" {
		t.Fatalf("scope after a later turn = %v in %q, want [alice] in no task", got.chain, got.task)
	}

	svc := "SST" + strings.Repeat("cd", 32)
	st.RegisterService(svc, "alice", "/ws")
	if got := dispatchScope(t, st, svc, nil); got.chain != nil {
		t.Fatalf("service token scope = %v, want none", got.chain)
	}
}

// While an asked turn is the one turn on the token, a tool's ForUser output
// is dropped rather than sent to the chat an earlier turn answered.
func TestDispatch_AskSourceNeverReachesAChat(t *testing.T) {
	st := newSessionTokenStore()
	tok := st.Issue("alice", "agent:alice:main", "/ws")
	st.BeginTurn("agent:alice:main", "telegram", "chat-1")() // an earlier chat turn
	defer st.BeginTurn("agent:alice:main", constants.AgentMessageChannel, "ask-1")()

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	dispatchScope(t, st, tok, msgBus)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if out, ok := msgBus.SubscribeOutbound(ctx); ok {
		t.Fatalf("an asked turn's ForUser was published: %+v", out)
	}
}

// A CLI agent's tool calls are billed to the inherited code of the turn
// holding the token, and change that turn's own code; a turn billed to its
// session's code carries none.
func TestDispatch_CarriesTurnBilling(t *testing.T) {
	st := newSessionTokenStore()
	tok := st.Issue("alice", "agent:alice:main", "/ws")
	run := tools.InheritedBillingOf(tools.WithBilling(context.Background(), "acme"))
	st.SetTurnScope("agent:alice:main", nil, "", run)
	got := dispatchScopeTool(t, st, tok, nil, &scopeTool{set: "acme-2"})
	if got.billing != "acme" {
		t.Fatalf("call billed %q, want acme", got.billing)
	}
	if run.Code() != "acme-2" {
		t.Errorf("the turn's code = %q, want acme-2", run.Code())
	}
	st.SetTurnScope("agent:alice:main", nil, "", nil)
	if got := dispatchScope(t, st, tok, nil); got.billing != "" {
		t.Errorf("session-billed turn's call carries %q", got.billing)
	}
}
