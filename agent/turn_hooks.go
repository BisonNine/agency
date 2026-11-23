// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// This file is the host side of the turn engine (engine_*.go): today's turn
// behaviour expressed as the engine's callbacks and event consumers, all
// registered in turnHooksFor, in the one order the turn relies on.

// turnState is what a turn's consumers keep between events.
type turnState struct {
	// stream forwards partial text to a streaming channel; nil otherwise.
	// It, notifyFallback and breadcrumbs are set when the model loop starts
	// (startModelLoop).
	stream *streamCoalescer
	// notifyFallback tells the chat the turn moved to another model; nil
	// for a turn with no chat to tell.
	notifyFallback providers.FallbackNotify
	// breadcrumbs: each tool call is announced in the chat (/tools).
	breadcrumbs bool
	// completedTools feeds the progress heartbeat; stopProgress ends it.
	completedTools atomic.Int64
	stopProgress   func()
	// evicted collects the turn's context evictions for one notice at the end.
	mu      sync.Mutex
	evicted []ctxengine.EvictionEvent
	// streamActivity is stream_tool_activity as read for the current batch.
	streamActivity bool
	// replyLines and lastImages follow the turn's answers (ModelAnswer): the
	// reply lines AfterModel added over the turn, and the images of the last
	// answer.
	replyLines []string
	lastImages int
	// reply is what AfterTurn decided, for the consumers of TurnFinished,
	// and stopTyping whether the chat's typing indicator is cleared.
	reply      turnReply
	stopTyping bool
	// pendingSource: the turn's pending source was recorded when its model
	// loop started (with the session store's pending marker).
	pendingSource bool
}

// turnReply is the reply AfterTurn decided for a turn that answered.
type turnReply struct {
	decided bool
	// outcome is the outcome it decided; the reply is sent only when the
	// turn finished with it (storing the reply may still fail).
	outcome hook.Outcome
	// content is the text sent to the chat and returned to the caller.
	content string
	// imagesOnly: a reply of images alone, all already sent.
	imagesOnly bool
	// systemError: content is the host's text for an empty reply.
	systemError bool
	iterations  int
	images      int
}

// compactionNoticePlugin tells the turn's person of a compaction it waits on
// and of a request that overflowed the context window.
func (al *AgentLoop) compactionNoticePlugin() hook.Plugin {
	return hook.Plugin{Name: "compaction-notice", OnCompaction: func(ctx context.Context, ev hook.Compaction) {
		t := hostTurnOf(ctx)
		if t == nil {
			return
		}
		switch ev.Phase {
		case hook.CompactionStarted:
			al.compactionStarted(ctx, ctxengine.CompactionStart{
				SessionKey: ev.Scope.Session, Reason: ctxengine.CompactionReason(ev.Reason), Blocking: ev.Blocking,
			})
		case hook.CompactionOverflow:
			al.publishOverflowNotice(ctx, t.o)
		case hook.CompactionEvicted:
			// Collected by the evictions consumer for one notice at the end.
		}
	}}
}

// hostTurn is one turn of an instance as the host runs it on the SDK agent:
// what the turn's callbacks and consumers read. It rides on the context
// given to Run (withHostTurn), so the plugins, built once per agent, find
// the turn they serve on the context they are called with.
type hostTurn struct {
	// ctx is the context the turn was run on, before the agent's scope: the
	// reasoning is published on it, as it always was, not on the loop's.
	ctx context.Context
	// opts are the turn's options as BeforeTurn leaves them; o as the run
	// was given them (what BeforeTurn does not change).
	opts *processOptions
	o    processOptions
	// model is the turn's model choice; cm and mem its session context.
	model  *turnModel
	cm     ctxengine.ContextManager
	mem    *cogmem.Session
	st     *turnState
	images *turnImages
}

type hostTurnKey struct{}

// withHostTurn returns ctx carrying a turn of agent on opts, whose model
// choice is tm and session context cm and mem, for the SDK agent's
// callbacks.
func (al *AgentLoop) withHostTurn(ctx context.Context, agent *AgentInstance, opts processOptions, tm *turnModel, cm ctxengine.ContextManager, mem *cogmem.Session) (context.Context, *hostTurn) {
	t := &hostTurn{ctx: ctx, o: opts, model: tm, cm: cm, mem: mem, st: &turnState{}}
	t.opts = &opts
	model := ""
	if tm != nil {
		model = tm.model
	}
	t.images = &turnImages{al: al, agent: agent, opts: opts, model: model}
	return context.WithValue(ctx, hostTurnKey{}, t), t
}

// hostTurnOf is the turn on ctx; nil for a run the host did not start.
func hostTurnOf(ctx context.Context) *hostTurn {
	if t, ok := ctx.Value(hostTurnKey{}).(*hostTurn); ok {
		return t
	}
	return nil
}

// modelName is the turn's model, by model_name; "" without one.
func (t *hostTurn) modelName() string {
	if t.model == nil {
		return ""
	}
	return t.model.model
}

// noModel reports whether the turn's agent had no model to call.
func (t *hostTurn) noModel() bool {
	return t.model == nil || t.model.chain == nil || len(t.model.candidates) == 0
}

// turnPlugins are the callbacks and event consumers of agent's turns, built
// once for its SDK agent; each serves the turn on the context it is called
// with (hostTurnOf). A human agent has one: the person answers (BeforeTurn).
// What depends on order is each in one consumer, in sequence:
//
//   - each provider call: the "LLM finish" line, then the turn's usage,
//     then the dumps, then the reasoning (modelCall);
//   - the end of the turn: the stream is flushed, then the eviction notice
//     posted, then the heartbeat stopped, so no trailing partial text is lost
//     and no placeholder edit lands after the reply (endTurn); then the
//     store-fault alert, then the reply sent, then its "Response" line;
//   - each finished tool call: its statistics, then its audit row, after
//     the call (recordToolCall); nothing is audited before a call runs;
//   - AfterModel: the "LLM response" line is logged first, from the
//     answer as the model gave it; then generated images are saved and
//     delivered before the reasoning may stand in for an empty reply, and
//     both run before the engine stores the assistant message (the notes
//     are in it);
//   - BeforeModel: the session's pins follow a request built from the
//     session store (Rebuilt) before its media are resolved.
//
// The per-chat settings are read when the model loop starts (BeforeLoop,
// startModelLoop), which switches on the consumers registered last. No tool
// callback is registered: tool calls and results are not changed, and
// redaction stays on logged and rendered error text.
func (al *AgentLoop) turnPlugins(agent *AgentInstance) []hook.Plugin {
	if agent.HumanModel != "" {
		return []hook.Plugin{{Name: "person", BeforeTurn: func(ctx context.Context, _ *hook.Turn) (*hook.TurnResult, error) {
			t := hostTurnOf(ctx)
			if t == nil {
				return nil, nil
			}
			reply, err := al.runHumanTurn(ctx, agent, *t.opts)
			if err != nil {
				return nil, err
			}
			return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: reply}, nil
		}}}
	}
	var ps []hook.Plugin
	add := func(p hook.Plugin) { ps = append(ps, p) }

	add(hook.Plugin{Name: "session", BeforeTurn: func(ctx context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
		t := hostTurnOf(ctx)
		if t == nil {
			return nil, nil
		}
		return nil, al.prepareSession(ctx, agent, t.cm, t.opts, turn)
	}})
	add(hook.Plugin{Name: "model-call", OnEvent: func(evCtx context.Context, ev hook.Event) {
		t := hostTurnOf(evCtx)
		if t == nil {
			return
		}
		switch c := ev.(type) {
		case hook.ModelCall:
			// The reasoning is published on its own goroutine, which may
			// outlive the model loop: it gets the turn's context, as it always
			// has, not the loop's.
			al.modelCall(evCtx, t.ctx, agent, t.o, t.modelName(), c)
		case hook.ModelAnswer:
			t.st.replyLines = append(t.st.replyLines, c.ReplyLines...)
			t.st.lastImages = len(c.Response.Images)
		}
	}})
	// Model calls in flight are counted, so a replaced client closes only
	// after them (WaitTurns).
	add(hook.Plugin{Name: "in-flight", OnEvent: func(_ context.Context, ev hook.Event) {
		switch ev.(type) {
		case hook.ModelCallStarted:
			al.activeRequests.Add(1)
		case hook.ModelCall:
			al.activeRequests.Done()
		}
	}})
	add(al.compactionNoticePlugin())

	// First among AfterModel: the answer is logged as the model gave it.
	add(hook.Plugin{Name: "response-log", AfterModel: al.logModelResponse(agent)})
	add(hook.Plugin{Name: "images", AfterModel: func(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) {
		if t := hostTurnOf(ctx); t != nil {
			t.images.afterModel(ctx, call, resp)
		}
	}})
	add(hook.Plugin{Name: "reasoning-as-content", AfterModel: al.reasoningAsContent})
	// The only BeforeModel, and it must stay the first: a rebuilt request
	// (Rebuilt) is reported to the BeforeModel callbacks of one attempt
	// only, so one answering before it would leave the refs unresolved.
	add(hook.Plugin{Name: "request-media", BeforeModel: al.requestMediaPlugin()})

	add(hook.Plugin{Name: "tool-record", OnEvent: func(ctx context.Context, ev hook.Event) {
		t := hostTurnOf(ctx)
		if t == nil {
			return
		}
		if f, ok := ev.(hook.ToolCallFinished); ok {
			recordToolCall(ctx, agent, t.o, f)
		}
	}})
	add(hook.Plugin{Name: "batch", OnEvent: func(ctx context.Context, ev hook.Event) {
		t := hostTurnOf(ctx)
		if t == nil {
			return
		}
		st := t.st
		switch e := ev.(type) {
		case hook.ToolBatchStarted:
			al.publishNarration(ctx, t.o, e.Content)
		case hook.ToolBatchFinished:
			st.completedTools.Add(int64(e.Calls))
			st.streamActivity = al.GetConfig().Agents.Defaults.StreamToolActivity
		case hook.ToolResultStored:
			// Each tool's own output reaches the user only with
			// stream_tool_activity; otherwise the user gets just the final
			// answer.
			if st.streamActivity && !e.Result.Silent && e.Result.ForUser != "" {
				al.publishToolForUser(ctx, t.o, e.Call.Name, e.Result)
			}
			if len(e.Result.Media) > 0 {
				al.publishToolMedia(ctx, t.o, e.Call.Name, e.Result)
			}
		}
	}})
	add(hook.Plugin{Name: "vision-describe", DescribeImages: func(ctx context.Context, images hook.UnseenImages) (string, bool) {
		t := hostTurnOf(ctx)
		if t == nil {
			return "", false
		}
		return al.describeToolImages(ctx, agent, *t.opts, t.modelName(), images)
	}})
	add(hook.Plugin{Name: "reply", AfterTurn: func(ctx context.Context, _ *hook.Turn, r *hook.TurnResult) {
		if t := hostTurnOf(ctx); t != nil {
			al.decideModelReply(agent, *t.opts, t.st, r)
		}
	}})
	add(hook.Plugin{Name: "turn", OnEvent: func(evCtx context.Context, ev hook.Event) {
		t := hostTurnOf(evCtx)
		if t == nil {
			return
		}
		st := t.st
		switch e := ev.(type) {
		case hook.Compaction:
			if e.Phase == hook.CompactionEvicted {
				st.mu.Lock()
				for _, x := range e.Evictions {
					st.evicted = append(st.evicted, ctxengine.EvictionEvent{Tool: x.Tool, Resource: x.Resource, Bytes: x.Bytes, Reason: x.Reason})
				}
				st.mu.Unlock()
			}
		case hook.TurnFinished:
			al.turnFinished(evCtx, t.ctx, agent, t, e)
		}
	}})

	// When the model loop starts, the per-chat settings are read and the
	// consumers below switched on; each does nothing until then.
	add(hook.Plugin{Name: "loop-start", BeforeLoop: func(loopCtx context.Context, loop *hook.Loop) error {
		if t := hostTurnOf(loopCtx); t != nil {
			al.startModelLoop(loopCtx, agent, t.o, t.st, loop)
		}
		return nil
	}})
	add(hook.Plugin{Name: "stream", OnTextDelta: func(ctx context.Context, ev hook.TextDelta) {
		if t := hostTurnOf(ctx); t != nil && t.st.stream != nil {
			t.st.stream.Add(ev.Text)
		}
	}})
	add(hook.Plugin{Name: "fallback-notice", OnFallback: func(ctx context.Context, ev hook.Fallback) {
		if t := hostTurnOf(ctx); t != nil && t.st.notifyFallback != nil {
			t.st.notifyFallback(fallbackAttempts(ev.Passed), fallbackCandidate(ev.Next))
		}
	}})
	add(hook.Plugin{Name: "breadcrumbs", OnEvent: func(ctx context.Context, ev hook.Event) {
		t := hostTurnOf(ctx)
		if t == nil {
			return
		}
		if s, ok := ev.(hook.ToolCallStarted); ok && t.st.breadcrumbs {
			al.publishBreadcrumb(ctx, t.o, hookToolCall(s.Call))
		}
	}})
	return ps
}

// turnFinished ends turn t of agent (TurnFinished), in order: the model
// loop ended (endTurn), the typing stopped, the no-model finish line, the
// pending source cleared, the store-fault and save reports, then the reply
// sent. turnCtx is the turn's context, which the end and the reply are
// published on.
func (al *AgentLoop) turnFinished(evCtx, turnCtx context.Context, agent *AgentInstance, t *hostTurn, e hook.TurnFinished) {
	st, o := t.st, t.o
	al.endTurn(turnCtx, agent, o, st)
	if st.stopTyping {
		al.stopTyping(o.Channel, o.ChatID)
	}
	if errors.Is(e.Err, hook.ErrNoModelAvailable) && t.noModel() {
		// A turn with no model makes no provider call, but has always
		// written its "LLM finish" line.
		model := t.modelName()
		al.emitFinish(evCtx, agent, o, hook.ModelCall{Iteration: e.Iterations, Model: hook.ModelRef{Name: model, Model: model}, Err: errNoModelConfigured})
	}
	if st.pendingSource && (e.Err == nil || !errors.Is(e.Cause, hook.ErrShutdown)) {
		al.clearPendingTurnSource(agent.ID, o.SessionKey)
	}
	al.reportStoreRefusal(agent, o.SessionKey, e.Err)
	al.reportSessionSave(agent, o.SessionKey, e.TurnResult)
	al.sendModelReply(turnCtx, agent, o, st, e.TurnResult)
}

// startModelLoop sets up a turn's model loop when it starts (BeforeLoop),
// after its message is stored and the turn marked pending, on the context
// BeforeLoop is given (the scoped turn context the engine runs on, ended
// when the turn's run returns, after TurnFinished stopped the heartbeat and
// flushed the stream): it records the pending turn's source, reads the per-chat settings
// (whether the channel streams partial text, the fallback notice, the tool
// breadcrumbs) and starts the progress heartbeat.
func (al *AgentLoop) startModelLoop(ctx context.Context, agent *AgentInstance, opts processOptions, st *turnState, loop *hook.Loop) {
	// The session store has just marked the turn pending: its source is
	// recorded with it, so a restart's replay still owes the sender.
	al.recordPendingTurnSource(ctx, agent, opts)
	st.pendingSource = true
	// Partial-text streaming: a channel that opts in gets the model's text as
	// it is generated, coalesced into sentence/length chunks. One forwarder
	// serves every model call in the turn; any other turn asks for none.
	loop.TextDeltas = streamToolNarration && al.channelManager != nil && al.channelManager.SupportsStreaming(opts.Channel)
	if loop.TextDeltas {
		channel, chatID, messageID := opts.Channel, opts.ChatID, opts.MessageID
		st.stream = newStreamCoalescer(func(batch string) {
			al.channelManager.StreamDelta(ctx, channel, chatID, messageID, batch)
		})
	}
	st.notifyFallback = al.fallbackNotifier(ctx, opts)
	// Breadcrumbs go to a user chat, never the internal "system" channel.
	st.breadcrumbs = opts.Channel != "" && opts.Channel != "system" && opts.ChatID != "" && al.getShowToolActivity(agent, opts.SessionKey)
	// Progress heartbeat: keep a long turn visibly alive by editing its
	// placeholder every progress_interval with a running tool-call count.
	st.stopProgress = al.startProgressUpdates(ctx, opts.Channel, opts.ChatID,
		al.GetConfig().Agents.Defaults.GetProgressInterval(), &st.completedTools)
}

// requestMediaPlugin makes the session's pins follow the request, and then
// resolves its media refs for dispatch: images become base64 data URLs,
// other media local paths in the content. It acts on a request built from
// the session store (hook.ModelInfo.Rebuilt): the turn's first, and any
// later one rebuilt when the history was rewritten during the turn (a
// compaction, an eviction or an overflow retry), so a reference the rewrite
// removed is released at once. A request kept from the step before has its
// media resolved already, so nothing is read again.
func (al *AgentLoop) requestMediaPlugin() hook.BeforeModelFunc {
	return func(ctx context.Context, call *hook.ModelInfo) (*providers.LLMResponse, error) {
		if !call.Rebuilt {
			return nil, nil
		}
		scope, _ := hook.ScopeOf(ctx)
		// Must run before resolveMediaRefs replaces refs with data URLs.
		al.reconcileSessionPins(scope.Session, call.Request.Messages)
		maxMediaSize := al.GetConfig().Agents.Defaults.GetMaxMediaSize()
		call.Request.Messages = resolveMediaRefs(call.Request.Messages, al.mediaStore, maxMediaSize)
		return nil, nil
	}
}

// logModelResponse logs a dispatch's answer (AfterModel, registered before
// every other one, so it sees the answer unchanged): the "LLM response" line,
// with the reasoning only when content may be logged. It changes nothing.
// It reads the turn's options as BeforeTurn leaves them.
func (al *AgentLoop) logModelResponse(agent *AgentInstance) hook.AfterModelFunc {
	return func(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) {
		log := logger.SDK()
		if !log.Enabled(ctx, slog.LevelDebug) {
			return
		}
		t := hostTurnOf(ctx)
		if t == nil {
			return
		}
		opts := t.opts
		r := resp.Response
		args := []any{
			"component", "agent",
			"agent_id", agent.ID,
			"iteration", call.Iteration,
			"content_chars", len(r.Content),
			"tool_calls", len(r.ToolCalls),
			"reasoning_chars", len(r.Reasoning),
			"target_channel", al.targetReasoningChannelID(opts.Channel),
			"channel", opts.Channel,
		}
		// Reasoning text is response body content.
		if logger.GetLogMessageContent() {
			args = append(args, "reasoning", r.Reasoning)
		}
		log.DebugContext(ctx, "LLM response", args...)
	}
}

// modelCall handles one provider call: its "LLM finish" line, then for an
// answer the turn's usage, the dumps and the reasoning, in that order.
// turnCtx is the turn's context, which the reasoning is published on.
func (al *AgentLoop) modelCall(ctx, turnCtx context.Context, agent *AgentInstance, opts processOptions, model string, c hook.ModelCall) {
	al.emitFinish(ctx, agent, opts, c)
	if c.Err != nil || c.Response == nil {
		return
	}
	addTurnUsage(opts.UsageOut, c.Response, c.Model.Provider, model)
	al.dumpResponse(agent, opts, model, c)
	al.deliverReasoning(turnCtx, agent, opts, c.Response.Reasoning)
}

// endTurn ends a model loop: any buffered partial text that never hit a
// boundary is streamed, then the eviction notice posted, then the heartbeat
// stopped.
func (al *AgentLoop) endTurn(ctx context.Context, agent *AgentInstance, opts processOptions, st *turnState) {
	// Last, and also when a step before it panics: the heartbeat's
	// goroutine stops only when told to.
	if st.stopProgress != nil {
		defer st.stopProgress()
	}
	if st.stream != nil {
		st.stream.Flush()
	}
	st.mu.Lock()
	evicted := st.evicted
	st.mu.Unlock()
	al.publishEvictionNotice(ctx, agent, opts, evicted)
}

// runModelTurn runs one turn of agent on opts on its SDK agent, its whole
// span: the message stored, the model loop, the reply decided, stored and,
// when opts asks, sent. It returns the reply (empty when nothing is to be
// said) or the error the turn ended with. A step-changing callback's panic,
// and the engine's own, end the turn as a panic in the turn always has: it
// reaches the caller, on its goroutine, once the turn has finished.
func (al *AgentLoop) runModelTurn(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
) (string, error) {
	tm := opts.turnModel
	if tm == nil {
		tm = al.selectTurnModel(agent, opts.SessionKey)
	}
	// A retried message is already in history, unless a single-shot turn
	// starts blank and stores it again.
	return al.runOnAgent(ctx, agent, opts, tm, cm, mem, opts.IsRetry && !agent.Spec.SingleShot())
}

// runOnAgent runs a turn of agent on opts on its SDK agent and waits for it:
// tm is its model choice, cm and mem its session context (nil for a human
// agent's turn), alreadyStored whether its message is in the session. The
// turn's run is recorded for /cancel (claimTurnHandle) before it can end.
func (al *AgentLoop) runOnAgent(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
	tm *turnModel,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
	alreadyStored bool,
) (string, error) {
	t, res, err := al.runTurnOnAgent(ctx, agent, opts, tm, cm, mem, alreadyStored)
	if err != nil {
		return "", err
	}
	return al.turnOutcome(t, res)
}

// runTurnOnAgent is runOnAgent up to the turn's result: the turn and how it
// ended, or the error of an agent that could not be built.
func (al *AgentLoop) runTurnOnAgent(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
	tm *turnModel,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
	alreadyStored bool,
) (*hostTurn, hook.TurnResult, error) {
	a, err := al.turnAgentOf(agent)
	if err != nil {
		return nil, hook.TurnResult{}, err
	}
	ctx, t := al.withHostTurn(ctx, agent, opts, tm, cm, mem)
	runOpts := []hook.RunOption{hook.AtDepth(toolsagents.SpawnDepth(ctx))}
	if alreadyStored {
		runOpts = append(runOpts, hook.AlreadyStored())
	}
	if id := turnIDFrom(ctx); id != "" {
		runOpts = append(runOpts, hook.WithTurnID(id))
	} else {
		// A run with no turn id of its own (one started over MCP) has none.
		runOpts = append(runOpts, hook.WithoutTurnID())
	}
	record := claimTurnHandle(ctx)
	run := a.Run(ctx, opts.SessionKey, turnMessage(opts), runOpts...)
	if record != nil {
		record(&runHandle{al: al, agent: agent, run: run, turn: t})
	}
	return t, run.Wait(), nil
}

// turnOutcome is what a finished turn returns to the host: its reply, or
// its error in the host's words.
func (al *AgentLoop) turnOutcome(t *hostTurn, res hook.TurnResult) (string, error) {
	err := res.Err
	if perr, ok := errors.AsType[*hook.PanicError](err); ok {
		panic(&callbackPanicError{perr: perr})
	}
	if eperr, ok := errors.AsType[*loop.EnginePanicError](err); ok {
		panic(&enginePanicError{perr: eperr})
	}
	if se, ok := errors.AsType[*hook.SessionStoreError](err); ok {
		return "", storeRefusalError(se.What, se.Err)
	}
	if errors.Is(err, hook.ErrNoModelAvailable) && t.noModel() {
		return "", noModelConfiguredError{err: err}
	}
	if err != nil {
		return "", err
	}
	return res.Reply, nil
}

// errNoModelConfigured is the error of a turn whose agent has no model to
// call (an empty chain), in the words a configuration without models has
// always been answered with; a turn's error matches it (errors.Is).
var errNoModelConfigured = errors.New("no model configured — enable a model and add your API key in the configuration")

// noModelConfiguredError is the engine's no-model error as the host returns
// it: the same text, matching errNoModelConfigured.
type noModelConfiguredError struct{ err error }

func (e noModelConfiguredError) Error() string        { return e.err.Error() }
func (e noModelConfiguredError) Unwrap() error        { return e.err }
func (e noModelConfiguredError) Is(target error) bool { return target == errNoModelConfigured }

// callbackPanicError is a step-changing callback's panic going on to the turn's
// caller: it reads as the panic's value, as the panic always has, and keeps
// the stack of the callback that panicked.
type callbackPanicError struct{ perr *hook.PanicError }

func (p *callbackPanicError) Error() string { return fmt.Sprintf("%v", p.perr.Value) }
func (p *callbackPanicError) Unwrap() error { return p.perr }

// enginePanicError is a panic in the turn engine itself (or in the host code
// it calls) going on to the turn's caller: it reads as the panic's value and
// keeps the stack of the code that panicked.
type enginePanicError struct{ perr *loop.EnginePanicError }

func (p *enginePanicError) Error() string { return fmt.Sprintf("%v", p.perr.Value) }
func (p *enginePanicError) Unwrap() error { return p.perr }

// panicReport is a recovered panic's value and stack as the turn's panic
// line logs them: a callback's or the engine's panic with the stack of the
// code that panicked.
func panicReport(r any) (value, stack string) {
	switch p := r.(type) {
	case *callbackPanicError:
		return fmt.Sprintf("%v", p.perr.Value), string(p.perr.Stack)
	case *enginePanicError:
		return fmt.Sprintf("%v", p.perr.Value), string(p.perr.Stack)
	}
	return fmt.Sprintf("%v", r), string(debug.Stack())
}

// turnMessage is the message a turn on opts answers, as the run is given
// it: its text, media, source and kind.
func turnMessage(opts processOptions) hook.Message {
	msg := hook.Message{Text: opts.UserMessage, Media: opts.Media}
	if opts.SenderName != "" {
		msg.Source = opts.SenderName
	} else if opts.SenderID != "" {
		msg.Source = opts.SenderID
	}
	if strings.HasPrefix(opts.SenderID, "callback") {
		msg.Kind = "callback"
	}
	return msg
}

// fileEditRepeatHint is the steering a model repeating a file_edit call
// gets on top of the engine's.
func fileEditRepeatHint(toolNames []string) string {
	if slices.Contains(toolNames, "file_edit") {
		return "For file_edit specifically: the file may have changed since you last read it, or your old_text does not match exactly (e.g. whitespace/indentation) — re-read it with file_read_lines and correct the old_text."
	}
	return ""
}

// modelFacts is model (a model_name) as a request of agent sees it: its
// limits for the agent (agent may be nil), its vision mode (VisionOff for a
// model the configuration does not have) and whether its provider requires
// reasoning content. Name and Model are model.
func (al *AgentLoop) modelFacts(agent *AgentInstance, model string) models.Candidate {
	c := models.Candidate{Name: model, Model: model, Limits: modelspec.Limits{Vision: config.VisionOff, ThinkingLevel: modelspec.ThinkingOff}}
	if agent != nil {
		l := agent.limitsFor(model)
		c.Limits.MaxTokens, c.Limits.ThinkingLevel = l.maxTokens, l.thinkingLevel
	}
	cfg := al.GetConfig()
	mc, err := cfg.GetModelConfig(model)
	if err != nil || mc == nil {
		return c
	}
	c.Limits.Vision = mc.Vision
	if mc.Provider != "" {
		if prov, perr := cfg.GetProvider(mc.Provider); perr == nil && prov != nil {
			c.ProviderSpec.RequireReasoningContent = prov.RequireReasoningContent
		}
	}
	return c
}

// fallbackAttempts are the models a Fallback event passed over, as the
// fallback notice renders them.
func fallbackAttempts(passed []hook.Attempt) []providers.FallbackAttempt {
	out := make([]providers.FallbackAttempt, 0, len(passed))
	for _, a := range passed {
		reason := a.Reason
		if a.Disabled && reason == "disabled" {
			reason = "" // the engine's name for what models leaves unnamed
		}
		out = append(out, providers.FallbackAttempt{
			Name: a.Model.Name, Provider: a.Model.Provider, Model: a.Model.Model,
			Err: a.Err, Reason: spawnllm.FailoverReason(reason), Duration: a.Duration,
			Skipped: a.Skipped, Disabled: a.Disabled, Remaining: a.Remaining,
		})
	}
	return out
}

// fallbackCandidate is the model a Fallback event moves to.
func fallbackCandidate(m hook.ModelRef) providers.FallbackCandidate {
	return models.Candidate{Name: m.Name, Provider: m.Provider, Protocol: m.Protocol, Model: m.Model}
}

// hookToolCall is call in the provider's form, for the breadcrumb and the
// audit row.
func hookToolCall(call hook.ToolCall) providers.ToolCall {
	return providers.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}
}

// emitFinish writes the "LLM finish" line for one provider call: the config
// provider and model_name that were called, and the model id sent to it.
func (al *AgentLoop) emitFinish(ctx context.Context, agent *AgentInstance, opts processOptions, c hook.ModelCall) {
	kind := llmKindAgent
	if agent.Spec.IsClone() {
		kind = llmKindSubagent
	}
	emitLLMFinishEvent(llmFinish{
		caller:         callerOf(agent),
		kind:           kind,
		iteration:      c.Iteration,
		ourProvider:    c.Model.Provider,
		ourModel:       c.Model.Name,
		requestedModel: c.Model.Model,
		sessionKey:     opts.SessionKey,
		channel:        opts.Channel,
		chatID:         opts.ChatID,
		turnID:         turnIDFrom(ctx),
		elapsedMs:      c.Elapsed.Milliseconds(),
		resp:           c.Response,
		err:            c.Err,
	})
}

// dumpResponse writes the diagnostic dumps of an answered model call: the
// refusal dump for a refusal when configured, else every response with
// dump_all.
func (al *AgentLoop) dumpResponse(agent *AgentInstance, opts processOptions, model string, c hook.ModelCall) {
	cfg := al.GetConfig()
	if al.dumpsDir.Path == "" || cfg == nil {
		return
	}
	var sent []providers.Message
	if c.Request != nil {
		sent = c.Request.Messages
	}
	if c.Response.FinishReason == "refusal" && cfg.Logging.DumpRefusals {
		al.dumpRefusal(agent, sent, c.Response, opts, model, c.Iteration)
	} else if cfg.Logging.DumpAll {
		al.dumpAll(agent, sent, c.Response, opts, model, c.Iteration)
	}
}

// deliverReasoning delivers a response's reasoning only when the session
// opted in (/reasoning on): to the configured reasoning channel if one
// exists, else inline in the main chat so the toggle works on any channel.
func (al *AgentLoop) deliverReasoning(ctx context.Context, agent *AgentInstance, opts processOptions, reasoning string) {
	if !al.getExposeReasoning(agent, opts.SessionKey) {
		return
	}
	reasoningTarget := al.targetReasoningChannelID(opts.Channel)
	inline := reasoningTarget == ""
	if inline {
		reasoningTarget = opts.ChatID
	}
	go al.handleReasoning(ctx, reasoning, opts.Channel, reasoningTarget, inline)
}

// reasoningAsContent lets the reasoning stand in for an empty final reply
// when the operator opts in (show_reasoning_as_content). Otherwise a model
// degenerating into reasoning-only output must not leak its raw
// chain-of-thought to the user, so empty content takes the graceful
// empty-response path instead. "Thinking" may arrive as reasoning_content
// (OpenAI-style) or reasoning (OpenRouter-style).
func (al *AgentLoop) reasoningAsContent(_ context.Context, _ *hook.ModelInfo, resp *hook.ModelResponse) {
	r := resp.Response
	if len(r.ToolCalls) > 0 || r.Content != "" {
		return
	}
	reasoningText := r.ReasoningContent
	if reasoningText == "" {
		reasoningText = r.Reasoning
	}
	if cfg := al.GetConfig(); reasoningText != "" && cfg != nil && cfg.Agents.Defaults.ShowReasoningAsContent {
		r.Content = reasoningText
	}
}

// publishOverflowNotice tells the turn's chat that its request overflowed the
// context window and is being compressed.
func (al *AgentLoop) publishOverflowNotice(ctx context.Context, opts processOptions) {
	if constants.IsInternalChannel(opts.Channel) {
		return
	}
	if pubErr := al.bus.PublishOutbound(ctx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: "Context window exceeded. Compressing history and retrying...",
	}); pubErr != nil {
		logger.WarnCF("agent", "Failed to publish compression notice",
			map[string]any{"error": pubErr.Error(), "channel": opts.Channel})
	}
}

// publishEvictionNotice posts the turn's context evictions as one line when
// the agent's policy has notify_user on. An agent that re-reads the same file
// every iteration would otherwise flood the chat; the per-eviction detail is
// always in the DEBUG log.
func (al *AgentLoop) publishEvictionNotice(ctx context.Context, agent *AgentInstance, opts processOptions, evicted []ctxengine.EvictionEvent) {
	if len(evicted) == 0 || opts.Channel == "" || !al.evictionNotifyUser(agent) {
		return
	}
	if err := al.bus.PublishOutbound(ctx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: summarizeEvictions(evicted),
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish eviction notice",
			map[string]any{"error": err.Error(), "channel": opts.Channel})
	}
}

// publishNarration posts the text a model sent alongside its tool calls, only
// when stream_tool_activity is on: by default the user sees the final answer,
// not the model's "let me also check…" play-by-play.
func (al *AgentLoop) publishNarration(ctx context.Context, opts processOptions, content string) {
	if content == "" || opts.Channel == "" || !al.GetConfig().Agents.Defaults.StreamToolActivity {
		return
	}
	pubCtx, pubCancel := context.WithTimeout(ctx, publishTimeout)
	defer pubCancel()
	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: content,
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish inter-tool narration",
			map[string]any{"error": err.Error(), "channel": opts.Channel})
	}
}

// recordToolCall records a finished call in the session's tool statistics
// and the audit log. A call whose tool panicked counts as its error text and
// has no audit row, as it never had.
func recordToolCall(ctx context.Context, agent *AgentInstance, opts processOptions, f hook.ToolCallFinished) {
	if f.Result == nil {
		return
	}
	if panicked := new(*hook.ToolPanicError); errors.As(f.Result.Err, panicked) {
		tools.RecordToolResult(opts.SessionKey, f.Call.Name, tools.ErrorResult(f.Result.ForLLM))
		return
	}
	tools.RecordToolResult(opts.SessionKey, f.Call.Name, f.Result)
	recordToolCallAudit(ctx, agent, opts, hookToolCall(f.Call), f.Result, f.Elapsed)
}

// describeToolImages describes, for a model that cannot see them, the images
// a batch of tools returned, through the agent's vision side-model. Nothing
// is added without one: the model gets only the hidden-attachment note.
func (al *AgentLoop) describeToolImages(ctx context.Context, agent *AgentInstance, opts processOptions, model string, images hook.UnseenImages) (string, bool) {
	if len(agent.VisionClients) == 0 {
		return "", false
	}
	focusParts := slices.Clone(images.Focus)
	if lu := strings.TrimSpace(opts.UserMessage); lu != "" {
		focusParts = append(focusParts, "User's request: "+lu)
	}
	desc, ok := al.describeImages(ctx, agent, images.Images, strings.Join(focusParts, "\n"))
	content := "An image was returned by a tool, but this model cannot view images and no description could be produced."
	if ok {
		content = "Image(s) described for you (this model cannot view images):\n" + desc
	}
	logger.InfoCF("agent", "injected vision description for non-vision model",
		map[string]any{"agent_id": agent.ID, "model": model, "images": len(images.Images), "described": ok})
	return content, true
}

// isCLIModel reports whether the model named name runs on a CLI provider.
func (al *AgentLoop) isCLIModel(name string) bool {
	cfg := al.GetConfig()
	mc, err := cfg.GetModelConfig(name)
	if err != nil || mc == nil {
		return false
	}
	prov, err := cfg.GetProvider(mc.Provider)
	return err == nil && prov != nil && config.IsCLIProtocol(prov.Protocol)
}
