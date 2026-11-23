// ClawEh
// License: MIT

package agent

import (
	"context"
	"slices"
	"sync"
	"testing"

	sdkagent "go.bison9.com/agent"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// probedAgent gives agent an SDK agent of its own whose first plugin
// records, for each turn, how its model loop ended (before AfterTurn decides
// the reply), so a test can read it. It replaces the instance's agent and
// closes the one it replaced.
func (al *AgentLoop) probedAgent(agent *AgentInstance, extra []hook.Plugin, specOpt func(*loop.Spec)) (func(*hostTurn) modelReply, error) {
	var mu sync.Mutex
	ends := map[*hostTurn]modelReply{}
	probe := hook.Plugin{Name: "probe", AfterTurn: func(ctx context.Context, _ *hook.Turn, r *hook.TurnResult) {
		if t := hostTurnOf(ctx); t != nil && r.Err == nil && !r.Skipped {
			mu.Lock()
			ends[t] = modelReplyOf(*r, t.st)
			mu.Unlock()
		}
	}}
	ended := func(t *hostTurn) modelReply {
		mu.Lock()
		defer mu.Unlock()
		return ends[t]
	}
	a, err := sdkagent.New(agent.ID, al.turnAgentSpec(agent), func(s *loop.Spec) {
		s.Plugins = append(append([]hook.Plugin{probe}, s.Plugins...), extra...)
		if specOpt != nil {
			specOpt(s)
		}
	})
	if err != nil {
		return nil, err
	}
	agent.turnAgentMu.Lock()
	prev := agent.turnAgent
	agent.turnAgent = a
	agent.turnAgentMu.Unlock()
	if prev != nil {
		if err := prev.Close(context.Background()); err != nil {
			return nil, err
		}
	}
	return ended, nil
}

// testTurn is a whole turn of agent on opts on its SDK agent, with tm as the
// turn's model choice (none when nil) and cm its session context (a new
// in-memory one when nil). It answers opts.UserMessage ("hi" when empty), or
// with messages the first request is messages, as given (already stored).
// al defaults to a loop on an empty configuration. It returns how the model
// loop ended and the turn's reply and error as the host sees them.
func testTurn(tb testing.TB, al *AgentLoop, agent *AgentInstance, opts processOptions, tm *turnModel, cm ctxengine.ContextManager, messages ...providers.Message) (modelReply, string, error) {
	tb.Helper()
	end, _, reply, err := runTestTurn(tb, context.Background(), al, agent, opts, tm, cm, nil, nil, messages...)
	return end, reply, err
}

// runTestTurn is testTurn on ctx, with extra plugins after the agent's own
// and specOpt (when set) changing the agent's settings, also returning the
// turn's result.
func runTestTurn(tb testing.TB, ctx context.Context, al *AgentLoop, agent *AgentInstance, opts processOptions, tm *turnModel, cm ctxengine.ContextManager, extra []hook.Plugin, specOpt func(*loop.Spec), messages ...providers.Message) (modelReply, hook.TurnResult, string, error) {
	tb.Helper()
	if al == nil {
		al = &AgentLoop{cfg: &config.Config{}}
	}
	if tm == nil {
		tm = &turnModel{}
	}
	if opts.SessionKey == "" {
		opts.SessionKey = "agent:" + agent.ID + ":main"
	}
	if opts.UserMessage == "" {
		opts.UserMessage = "hi"
	}
	if agent.Sessions == nil {
		agent.Sessions = session.NewMemory()
	}
	if cm == nil {
		cm = ctxengine.New(opts.SessionKey, agent.Sessions, ctxengine.WithLogger(logger.SDK()),
			ctxengine.WithCompactionStartFunc(loop.CompactionStart))
	}
	alreadyStored := false
	if len(messages) > 0 {
		cm = &firstAssembly{ContextManager: cm, first: messages}
		alreadyStored = true
	}
	ended, err := al.probedAgent(agent, extra, specOpt)
	if err != nil {
		tb.Fatal(err)
	}
	opts.turnModel = tm
	ctx = providers.WithAgentID(ctx, agent.ID)
	ht, res, err := al.runTurnOnAgent(ctx, agent, opts, tm, cm, nil, alreadyStored)
	if err != nil {
		tb.Fatal(err)
	}
	reply, err := al.turnOutcome(ht, res)
	return ended(ht), res, reply, err
}

// runLLMIteration runs a whole turn of agent on opts whose first request is
// messages, as given (the message already stored), and returns how its model
// loop ended: the model loop over messages, with the turn's callbacks and
// consumers, as tests of the loop drive it.
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
	tm := opts.turnModel
	if tm == nil {
		tm = al.selectTurnModel(agent, opts.SessionKey)
	}
	opts.turnModel = tm
	ended, err := al.probedAgent(agent, nil, nil)
	if err != nil {
		return modelReply{}, err
	}
	ht, res, err := al.runTurnOnAgent(ctx, agent, opts, tm, &firstAssembly{ContextManager: cm, first: messages}, mem, true)
	if err != nil {
		return modelReply{}, err
	}
	if _, herr := al.turnOutcome(ht, res); herr != nil {
		return modelReply{iterations: res.Iterations}, herr
	}
	return ended(ht), nil
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

// assemblyRecorder is a context manager that records the tool-schema cost
// (ToolDefinitionTokens) of every assembly a turn asks it for.
type assemblyRecorder struct {
	ctxengine.ContextManager
	mu         sync.Mutex
	toolTokens []int
}

func (r *assemblyRecorder) Assemble(ctx context.Context, req ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	r.mu.Lock()
	r.toolTokens = append(r.toolTokens, req.ToolDefinitionTokens)
	r.mu.Unlock()
	return r.ContextManager.Assemble(ctx, req)
}

// tokens are the recorded costs, one per assembly, in order.
func (r *assemblyRecorder) tokens() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.toolTokens)
}

// testHooks are an agent's turn plugins (turnPlugins) serving one turn,
// for tests that drive them with events: each call gets the turn's context.
type testHooks struct {
	ctx context.Context
	ps  []hook.Plugin
	// onTextDelta are the plugins' OnTextDelta callbacks.
	onTextDelta []hook.OnTextDeltaFunc
	// panics are the consumers' panics, skipped.
	panics []any
}

// emit gives ev to every OnEvent consumer, in order, on the turn's context.
func (h *testHooks) emit(ctx context.Context, ev hook.Event) {
	ctx = h.onTurn(ctx)
	for _, p := range h.ps {
		if p.OnEvent != nil {
			h.observe(func() { p.OnEvent(ctx, ev) })
		}
	}
}

// onTurn is ctx carrying the turn the hooks serve.
func (h *testHooks) onTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, hostTurnKey{}, hostTurnOf(h.ctx))
}

// observe runs fn, recording and skipping its panic, as the agent runs a
// consumer.
func (h *testHooks) observe(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			h.panics = append(h.panics, r)
		}
	}()
	fn()
}

// callBeforeLoop runs the BeforeLoop callbacks, in order, on the turn's
// context.
func (h *testHooks) callBeforeLoop(ctx context.Context, loop *hook.Loop) error {
	ctx = h.onTurn(ctx)
	for _, p := range h.ps {
		if p.BeforeLoop != nil {
			if err := p.BeforeLoop(ctx, loop); err != nil {
				return err
			}
		}
	}
	return nil
}

// callAfterModel runs the AfterModel callbacks, in order, on the turn's
// context. A callback's panic propagates, as one ends the turn on the agent.
func (h *testHooks) callAfterModel(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) {
	ctx = h.onTurn(ctx)
	for _, p := range h.ps {
		if p.AfterModel != nil {
			p.AfterModel(ctx, call, resp)
		}
	}
}

// newTurnHooks are the plugins of agent serving one turn on opts, whose
// model is model, for tests that drive them with events.
func (al *AgentLoop) newTurnHooks(ctx context.Context, agent *AgentInstance, opts processOptions, model string) *testHooks {
	h, _ := al.turnHooksFor(ctx, agent, &opts, model, nil)
	return h
}

// buildTurnHooks is newTurnHooks with the turn's state, with the model loop
// started at once (the BeforeLoop callbacks run).
func (al *AgentLoop) buildTurnHooks(ctx context.Context, agent *AgentInstance, opts processOptions, model string) (*testHooks, *turnState) {
	h, st := al.turnHooksFor(ctx, agent, &opts, model, nil)
	if err := h.callBeforeLoop(ctx, &hook.Loop{TextDeltas: len(h.onTextDelta) > 0}); err != nil {
		panic(err)
	}
	return h, st
}

// turnHooksFor are agent's plugins serving a turn on *opts whose model is
// model and session context cm, with the turn's state.
func (al *AgentLoop) turnHooksFor(ctx context.Context, agent *AgentInstance, opts *processOptions, model string, cm ctxengine.ContextManager) (*testHooks, *turnState) {
	tctx, t := al.withHostTurn(ctx, agent, *opts, &turnModel{model: model}, cm, nil)
	h := &testHooks{ctx: tctx, ps: al.turnPlugins(agent)}
	for _, p := range h.ps {
		if p.OnTextDelta != nil {
			h.onTextDelta = append(h.onTextDelta, p.OnTextDelta)
		}
	}
	return h, t.st
}
