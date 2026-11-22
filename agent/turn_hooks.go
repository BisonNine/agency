// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

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
// registered in turnHooksFor (and, when the model loop starts,
// loopPlugins), in the one order the turn relies on.

// turnState is what a turn's consumers keep between events.
type turnState struct {
	// stream forwards partial text to a streaming channel; nil otherwise.
	stream *streamCoalescer
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
func (al *AgentLoop) compactionNoticePlugin(opts processOptions) hook.Plugin {
	return hook.Plugin{Name: "compaction-notice", OnCompaction: func(ctx context.Context, ev hook.Compaction) {
		switch ev.Phase {
		case hook.CompactionStarted:
			al.compactionStarted(ctx, ctxengine.CompactionStart{
				SessionKey: ev.Scope.Session, Reason: ctxengine.CompactionReason(ev.Reason), Blocking: ev.Blocking,
			})
		case hook.CompactionOverflow:
			al.publishOverflowNotice(ctx, opts)
		case hook.CompactionEvicted:
			// Collected by the evictions consumer for one notice at the end.
		}
	}}
}

// turnHooksFor registers the callbacks and event consumers of a turn of
// agent on *opts (which BeforeTurn may change), whose model is model. What
// depends on order is each in one consumer, in sequence:
//
//   - each provider call: the "LLM finish" line, then the turn's usage,
//     then the dumps, then the reasoning (modelCall);
//   - the end of the turn: the stream is flushed, then the eviction notice
//     posted, then the heartbeat stopped, so no trailing partial text is lost
//     and no placeholder edit lands after the reply (endTurn); then the
//     store-fault alert, then the reply sent, then its "Response" line;
//   - each finished tool call: its statistics, then its audit row, after
//     the call (recordToolCall); nothing is audited before a call runs;
//   - AfterModel: generated images are saved and delivered before the
//     reasoning may stand in for an empty reply, and both run before the
//     engine stores the assistant message (the notes are in it);
//   - BeforeModel: the session's pins follow the first request before its
//     media are resolved.
//
// The plugins that read per-chat settings are registered when the model
// loop starts (loopPlugins). No tool callback is registered: tool calls
// and results are not changed, and redaction stays on logged and rendered
// error text.
func (al *AgentLoop) turnHooksFor(ctx context.Context, agent *AgentInstance, opts *processOptions, model string, cm ctxengine.ContextManager) (*turnHooks, *turnState) {
	h := &turnHooks{log: logger.SDK()}
	st := &turnState{}
	o := *opts // what BeforeTurn does not change

	h.add(al.turnPlugin(agent, opts, cm))
	h.add(hook.Plugin{Name: "model-call", OnEvent: func(evCtx context.Context, ev hook.Event) {
		switch c := ev.(type) {
		case hook.ModelCall:
			// The reasoning is published on its own goroutine, which may
			// outlive the model loop: it gets the turn's context, as it always
			// has, not the loop's.
			al.modelCall(evCtx, ctx, agent, o, model, c)
		case hook.ModelAnswer:
			st.replyLines = append(st.replyLines, c.ReplyLines...)
			st.lastImages = len(c.Response.Images)
		}
	}})
	// Model calls in flight are counted, so a replaced client closes only
	// after them (WaitTurns).
	h.add(hook.Plugin{Name: "in-flight", OnEvent: func(_ context.Context, ev hook.Event) {
		switch ev.(type) {
		case hook.ModelCallStarted:
			al.activeRequests.Add(1)
		case hook.ModelCall:
			al.activeRequests.Done()
		}
	}})
	h.add(al.compactionNoticePlugin(o))

	images := &turnImages{al: al, agent: agent, opts: o, model: model}
	h.add(hook.Plugin{Name: "images", AfterModel: images.afterModel})
	h.add(hook.Plugin{Name: "reasoning-as-content", AfterModel: al.reasoningAsContent})
	h.add(hook.Plugin{Name: "first-request", BeforeModel: al.firstRequestPlugin()})

	h.add(hook.Plugin{Name: "tool-record", OnEvent: func(ctx context.Context, ev hook.Event) {
		if f, ok := ev.(hook.ToolCallFinished); ok {
			recordToolCall(ctx, agent, o, f)
		}
	}})
	h.add(hook.Plugin{Name: "batch", OnEvent: func(ctx context.Context, ev hook.Event) {
		switch e := ev.(type) {
		case hook.ToolBatchStarted:
			al.publishNarration(ctx, o, e.Content)
		case hook.ToolBatchFinished:
			st.completedTools.Add(int64(e.Calls))
			st.streamActivity = al.GetConfig().Agents.Defaults.StreamToolActivity
		case hook.ToolResultStored:
			// Each tool's own output reaches the user only with
			// stream_tool_activity; otherwise the user gets just the final
			// answer.
			if st.streamActivity && !e.Result.Silent && e.Result.ForUser != "" {
				al.publishToolForUser(ctx, o, e.Call.Name, e.Result)
			}
			if len(e.Result.Media) > 0 {
				al.publishToolMedia(ctx, o, e.Call.Name, e.Result)
			}
		}
	}})
	h.add(hook.Plugin{Name: "vision-describe", DescribeImages: func(ctx context.Context, images hook.UnseenImages) (string, bool) {
		return al.describeToolImages(ctx, agent, *opts, model, images)
	}})
	h.add(hook.Plugin{Name: "reply", AfterTurn: func(_ context.Context, _ *hook.Turn, r *hook.TurnResult) {
		al.decideModelReply(agent, *opts, st, r)
	}})
	h.add(hook.Plugin{Name: "turn", OnEvent: func(evCtx context.Context, ev hook.Event) {
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
			al.endTurn(ctx, agent, o, st)
			if st.stopTyping {
				al.stopTyping(o.Channel, o.ChatID)
			}
			if errors.Is(e.Err, errNoModelConfigured) {
				// A turn with no model makes no provider call, but has always
				// written its "LLM finish" line.
				al.emitFinish(evCtx, agent, o, hook.ModelCall{Iteration: e.Iterations, Model: hook.ModelRef{Name: model, Model: model}, Err: errNoModelConfigured})
			}
			if st.pendingSource && (e.Err == nil || !errors.Is(e.Cause, hook.ErrShutdown)) {
				al.clearPendingTurnSource(agent.ID, o.SessionKey)
			}
			al.reportStoreRefusal(agent, o.SessionKey, e.Err)
			al.reportSessionSave(agent, o.SessionKey, e.TurnResult)
			al.sendModelReply(ctx, agent, o, st, e.TurnResult)
		}
	}})
	return h, st
}

// loopPlugins returns what runs when the model loop of a turn starts, after
// its message is stored and the turn marked pending: it records the pending
// turn's source, starts the progress heartbeat and registers the plugins
// that read per-chat settings there (the streaming of partial text, the
// fallback notice and the tool breadcrumbs).
func (al *AgentLoop) loopPlugins(ctx context.Context, agent *AgentInstance, opts processOptions, st *turnState) func(context.Context) []hook.Plugin {
	return func(loopCtx context.Context) []hook.Plugin {
		// The session store has just marked the turn pending: its source is
		// recorded with it, so a restart's replay still owes the sender.
		al.recordPendingTurnSource(loopCtx, agent, opts)
		st.pendingSource = true
		var plugins []hook.Plugin
		// Partial-text streaming: a channel that opts in gets the model's
		// text as it is generated, coalesced into sentence/length chunks. One
		// forwarder serves every model call in the turn.
		if streamToolNarration && al.channelManager != nil && al.channelManager.SupportsStreaming(opts.Channel) {
			channel, chatID := opts.Channel, opts.ChatID
			st.stream = newStreamCoalescer(func(batch string) {
				al.channelManager.StreamDelta(ctx, channel, chatID, batch)
			})
			plugins = append(plugins, hook.Plugin{Name: "stream", OnTextDelta: func(_ context.Context, ev hook.TextDelta) { st.stream.Add(ev.Text) }})
		}
		if notify := al.fallbackNotifier(ctx, opts); notify != nil {
			plugins = append(plugins, hook.Plugin{Name: "fallback-notice", OnFallback: func(_ context.Context, ev hook.Fallback) {
				notify(fallbackAttempts(ev.Passed), fallbackCandidate(ev.Next))
			}})
		}
		// Breadcrumbs go to a user chat, never the internal "system" channel.
		if opts.Channel != "" && opts.Channel != "system" && opts.ChatID != "" && al.getShowToolActivity(agent, opts.SessionKey) {
			plugins = append(plugins, hook.Plugin{Name: "breadcrumbs", OnEvent: func(ctx context.Context, ev hook.Event) {
				if s, ok := ev.(hook.ToolCallStarted); ok {
					al.publishBreadcrumb(ctx, opts, hookToolCall(s.Call))
				}
			}})
		}
		// Progress heartbeat: keep a long turn visibly alive by editing its
		// placeholder every progress_interval with a running tool-call count.
		st.stopProgress = al.startProgressUpdates(ctx, opts.Channel, opts.ChatID,
			al.GetConfig().Agents.Defaults.GetProgressInterval(), &st.completedTools)
		return plugins
	}
}

// turnPlugin readies a turn of agent before anything of it is stored
// (BeforeTurn): a session_clear handoff releases the session's pins and
// reissues its token, a single-shot turn starts blank, whispers held for the
// agent open its message and the inbound media are described and pinned.
func (al *AgentLoop) turnPlugin(agent *AgentInstance, opts *processOptions, cm ctxengine.ContextManager) hook.Plugin {
	return hook.Plugin{
		Name: "session",
		BeforeTurn: func(ctx context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
			return nil, al.prepareSession(ctx, agent, cm, opts, turn)
		},
	}
}

// firstRequestPlugin makes the session's pins follow the turn's first
// request, as assembled, and then resolves its media refs for dispatch:
// images become base64 data URLs, other media local paths in the content.
// It acts once per turn, on the first dispatch.
func (al *AgentLoop) firstRequestPlugin() hook.BeforeModelFunc {
	var done bool
	return func(ctx context.Context, call *hook.ModelInfo) (*providers.LLMResponse, error) {
		if done {
			return nil, nil
		}
		done = true
		scope, _ := hook.ScopeOf(ctx)
		// Must run before resolveMediaRefs replaces refs with data URLs.
		al.reconcileSessionPins(scope.Session, call.Request.Messages)
		maxMediaSize := al.GetConfig().Agents.Defaults.GetMaxMediaSize()
		call.Request.Messages = resolveMediaRefs(call.Request.Messages, al.mediaStore, maxMediaSize)
		return nil, nil
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

// turnScope is the scope of a turn of agent on sessionKey: its accounting
// identity (a clone's is its source's), the session, the turn id and the
// sub-agent depth on ctx.
func (al *AgentLoop) turnScope(ctx context.Context, agent *AgentInstance, sessionKey string) hook.Scope {
	id := agent.Identity()
	return hook.Scope{
		AgentID:   id.Agent,
		AgentName: id.Name,
		Project:   id.Project,
		Session:   sessionKey,
		TurnID:    turnIDFrom(ctx),
		Depth:     toolsagents.SpawnDepth(ctx),
	}
}

// runModelTurn runs one turn of agent on opts through the turn engine, its
// whole span: the message stored, the model loop, the reply decided, stored
// and, when opts asks, sent. It returns the reply (empty when nothing is to
// be said) or the error the turn ended with. A step-changing callback's
// panic ends the turn as a panic in the turn always has: it reaches the
// caller once the turn has finished.
func (al *AgentLoop) runModelTurn(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
) (string, error) {
	turn, ctx := al.newHostEngineTurn(ctx, agent, nil, opts, cm, mem)
	_, err := turn.run(ctx)
	return al.turnOutcome(turn, err)
}

// turnOutcome is what a finished engine turn returns to the host: its reply,
// or its error in the host's words.
func (al *AgentLoop) turnOutcome(turn *engineTurn, err error) (string, error) {
	if perr, ok := errors.AsType[*hook.PanicError](err); ok {
		panic(perr.Value)
	}
	if se, ok := errors.AsType[*hook.SessionStoreError](err); ok {
		return "", storeRefusalError(se.What, se.Err)
	}
	if err != nil {
		return "", err
	}
	return turn.handle.result.Reply, nil
}

// newHostEngineTurn is the engine turn of agent on opts, with the turn's
// model choice (made now when opts has none) and its callbacks, over
// messages until the first request is assembled. The returned context
// carries the turn's scope and hooks.
func (al *AgentLoop) newHostEngineTurn(
	ctx context.Context,
	agent *AgentInstance,
	messages []providers.Message,
	opts processOptions,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
) (*engineTurn, context.Context) {
	tm := opts.turnModel
	if tm == nil {
		tm = al.selectTurnModel(agent, opts.SessionKey)
	}
	o := &opts
	hooks, st := al.turnHooksFor(ctx, agent, o, tm.model, cm)
	ctx = beginTurnContext(ctx, al.turnScope(ctx, agent, opts.SessionKey), hooks, al.clk())
	scope, _ := hook.ScopeOf(ctx)
	spec := engineSpec{
		agentID: agent.ID,
		scope:   scope,
		message: turnMessage(opts),
		options: hook.RunOptions{
			// A retried message is already in history, unless a single-shot
			// turn starts blank and stores it again.
			AlreadyStored: opts.IsRetry && !agent.Spec.SingleShot(),
			ResetSession:  opts.ResetSession,
			Depth:         scope.Depth,
		},
		loopPlugins: al.loopPlugins(ctx, agent, opts, st),

		maxIterations: agent.MaxIterations,
		toolTimeout:   func() time.Duration { return al.GetConfig().Agents.Defaults.GetToolTimeout() },
		retries:       llmMaxRetries,
		retryBackoff:  timeoutRetryBackoff,
		temperature:   agent.Temperature,
		repeatHint:    fileEditRepeatHint,

		clock:      al.clk(),
		log:        logger.SDK(),
		logContent: logger.GetLogMessageContent,
		redact:     redactSecrets,

		chain:          tm.chain,
		candidates:     tm.candidates,
		model:          tm.model,
		modelMaxTokens: tm.limits.maxTokens,
		contextWindow:  tm.limits.contextWindow,
		noTools:        tm.limits.noTools,
		direct:         tm.direct,
		directModel:    al.directModel(agent, tm),

		cm:    cm,
		mem:   mem,
		env:   &llmTurnEnv{al: al, agent: agent, opts: o, mem: mem},
		hooks: hooks,
	}
	if agent.Sessions != nil {
		spec.store = agent.Sessions
	}
	return newEngineTurn(spec, messages), ctx
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

// llmTurnEnv is the host's services as the engine reaches them for a turn
// of agent on opts.
type llmTurnEnv struct {
	al    *AgentLoop
	agent *AgentInstance
	// opts are the turn's options, as BeforeTurn left them.
	opts *processOptions
	mem  *cogmem.Session
}

func (e *llmTurnEnv) toolDefs() []spawnllm.ToolDefinition {
	return e.agent.Tools.ToolDefs(nil).Send
}

// executeTool runs a call with the agent's allow checker on its context (so
// execution enforces the tool allowlist again, defense in depth), and with
// the turn's reply chat, so files a tool sends go where the reply goes. An
// AsyncExecutor tool's late result is routed by asyncToolCallback.
func (e *llmTurnEnv) executeTool(ctx context.Context, name string, args map[string]any) *toolkit.Result {
	asyncCallback := e.al.asyncToolCallback(ctx, e.agent, *e.opts, name)
	execCtx := tools.WithToolAllowChecker(ctx, e.agent.Config)
	execCtx = tools.WithReplyChat(execCtx, tools.ReplyChat{Channel: e.opts.Channel, ChatID: e.opts.ChatID})
	return e.agent.Tools.ExecuteWithContext(execCtx, name, args, e.opts.Channel, e.opts.ChatID, asyncCallback)
}

func (e *llmTurnEnv) assembleRequest(ctx context.Context, defs []spawnllm.ToolDefinition, history []spawnllm.Message) ctxengine.AssembleRequest {
	return e.al.assembleRequestWithDefs(ctx, e.agent, e.mem, *e.opts, defs, history)
}

func (e *llmTurnEnv) firstRequest(ctx context.Context) ctxengine.AssembleRequest {
	return e.al.assembleRequest(ctx, e.agent, e.mem, *e.opts)
}

func (e *llmTurnEnv) compressionManager() (ctxengine.ContextManager, func()) {
	return e.al.getContextManager(e.agent, e.opts.SessionKey)
}

// sessionSummary is "" for an agent without a session store.
func (e *llmTurnEnv) sessionSummary() string {
	if e.agent.Sessions == nil {
		return ""
	}
	return e.agent.Sessions.GetSummary(e.opts.SessionKey)
}

func (e *llmTurnEnv) directClient(name string) (spawnllm.LLMProvider, func(), error) {
	return e.al.catalog.Client(name)
}

func (e *llmTurnEnv) responseLogFields() map[string]any {
	return map[string]any{
		"target_channel": e.al.targetReasoningChannelID(e.opts.Channel),
		"channel":        e.opts.Channel,
	}
}

func (e *llmTurnEnv) formatRequest(messages []spawnllm.Message, defs []spawnllm.ToolDefinition) (string, string) {
	return formatMessagesForLog(messages), formatToolsForLog(defs)
}

// directModel is the model a turn of agent on a direct chain (tm.direct)
// sends, with its facts; zero for any other turn.
func (al *AgentLoop) directModel(agent *AgentInstance, tm *turnModel) models.Candidate {
	if tm.direct == "" {
		return models.Candidate{}
	}
	c := al.modelFacts(agent, tm.model)
	if info, ok := al.catalog.Get(tm.direct); ok {
		c.Protocol = info.Provider.Protocol
	}
	return c
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
	if panicked := new(*toolPanicError); errors.As(f.Result.Err, panicked) {
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
