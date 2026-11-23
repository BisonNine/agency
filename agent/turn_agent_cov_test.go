// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// An instance whose SDK agent cannot be built gets no agent and the error
// names it.
func TestTurnAgentCov_BuildFails(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksCovLoop(t)
	inst := &AgentInstance{}
	a, err := al.turnAgentOf(inst)
	if a != nil || err == nil || !errors.Is(err, loop.ErrInvalidSpec) {
		t.Fatalf("turnAgentOf = %v, %v; want the invalid spec error", a, err)
	}
	if inst.turnAgent != nil {
		t.Fatal("a failed build left an agent on the instance")
	}
}

// The spec's callbacks do nothing for a context that carries no host turn,
// and an agent without an iteration limit has none.
func TestTurnAgentCov_SpecWithoutHostTurn(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksCovLoop(t)
	inst := al.GetRegistry().Default()
	inst.MaxIterations = 0
	var s loop.Spec
	al.turnAgentSpec(inst)(&s)
	ctx := context.Background()
	if got := s.TurnLimits(ctx, hook.Scope{}).MaxIterations; got != hook.NoIterations {
		t.Fatalf("MaxIterations = %d, want none", got)
	}
	if layers := s.InstructionLayers(ctx); layers != nil {
		t.Fatalf("InstructionLayers = %v, want none", layers)
	}
	if got := s.ToolContext(ctx, hook.ToolCall{Name: "x"}); got != nil {
		t.Fatalf("ToolContext = %v, want none", got)
	}
	s.AsyncResult(ctx, hook.ToolCall{Name: "x"}, toolkit.Info{}, &toolkit.Result{ForLLM: "late"})
	if _, err := s.Session(ctx, hook.Scope{}); !errors.Is(err, errNoTurnSession) {
		t.Fatalf("Session = %v, want no turn session", err)
	}

	inst.HumanModel = "Bob (human)"
	var hs loop.Spec
	al.turnAgentSpec(inst)(&hs)
	sess, err := hs.Session(ctx, hook.Scope{})
	if err != nil || sess.Context != nil {
		t.Fatalf("a person's Session = %+v, %v; want an empty session", sess, err)
	}
}

// The agent's tool registry reports its version and changes.
func TestTurnAgentCov_ToolsVersionAndSubscribe(t *testing.T) {
	r := tools.NewToolRegistry()
	at := agentTools{r: r}
	before := at.Version()
	changes := make(chan toolkit.Change, 4)
	stop := at.Subscribe(func(c toolkit.Change) { changes <- c })
	defer stop()
	r.Register(&inboundCovMessageTool{})
	recv(t, changes, "the subscriber to see the new tool")
	if at.Version() == before {
		t.Fatal("the version did not change with a new tool")
	}
}
