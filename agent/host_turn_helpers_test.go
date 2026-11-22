// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// hostTurn is the engine turn the host builds for agent on opts, over
// messages, with tm as the turn's model choice (none when nil): the turn
// runLLMIteration runs, its callbacks registered. al defaults to a loop on an
// empty configuration.
func hostTurn(tb testing.TB, al *AgentLoop, agent *AgentInstance, opts processOptions, tm *turnModel, messages ...providers.Message) *engineTurn {
	tb.Helper()
	if al == nil {
		al = &AgentLoop{cfg: &config.Config{}}
	}
	if tm == nil {
		tm = &turnModel{}
	}
	opts.turnModel = tm
	turn, _ := al.newHostEngineTurn(context.Background(), agent, messages, opts, nil, nil)
	return turn
}

// newTestDispatch is the dispatch of turn's next request, with no tools.
func newTestDispatch(turn *engineTurn) *llmDispatch {
	return turn.newDispatch(context.Background(), nil, 0)
}

// runLLMIteration runs a whole engine turn of agent on opts whose first
// request is messages, as given (the message already stored), and returns
// how its model loop ended: the model loop over messages, with this turn's
// callbacks and consumers, as tests of the loop drive it.
func (al *AgentLoop) runLLMIteration(
	ctx context.Context,
	agent *AgentInstance,
	messages []providers.Message,
	opts processOptions,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
) (modelReply, error) {
	ctx = providers.WithAgentID(ctx, agent.ID)
	opts.IsRetry = true
	turn, ctx := al.newHostEngineTurn(ctx, agent, nil, opts, &firstAssembly{ContextManager: cm, first: messages}, mem)
	turn.spec.options.AlreadyStored = true
	res, err := turn.run(ctx)
	if _, herr := al.turnOutcome(turn, err); herr != nil {
		return modelReply{iterations: res.iterations}, herr
	}
	return modelReply{
		content:      res.content,
		normal:       res.normal,
		degenerate:   res.degenerate,
		finishReason: res.finishReason,
		iterations:   res.iterations,
		images:       res.images,
		imageReplies: res.replyLines,
		lastImages:   res.lastImages,
	}, nil
}

// firstAssembly is a context manager whose first assembly is first, as
// given; every other call is the manager's own.
type firstAssembly struct {
	ctxengine.ContextManager
	first []providers.Message
	done  bool
}

func (f *firstAssembly) Assemble(ctx context.Context, req ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	if !f.done {
		f.done = true
		return ctxengine.Assembly{Messages: f.first}, nil
	}
	return f.ContextManager.Assemble(ctx, req)
}

// newTurnHooks registers the callbacks and event consumers of one turn of
// agent on opts, whose model is model, for tests that drive them with events.
func (al *AgentLoop) newTurnHooks(ctx context.Context, agent *AgentInstance, opts processOptions, model string) *turnHooks {
	h, _ := al.buildTurnHooks(ctx, agent, opts, model)
	return h
}

// buildTurnHooks is newTurnHooks with the consumers' state, with the model
// loop started at once (the BeforeLoop callbacks run).
func (al *AgentLoop) buildTurnHooks(ctx context.Context, agent *AgentInstance, opts processOptions, model string) (*turnHooks, *turnState) {
	h, st := al.turnHooksFor(ctx, agent, &opts, model, nil)
	if err := h.callBeforeLoop(ctx, &hook.Loop{TextDeltas: len(h.onTextDelta) > 0}); err != nil {
		panic(err)
	}
	return h, st
}
