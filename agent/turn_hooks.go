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
// registered in newTurnHooks, in the one order the turn relies on.

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
}

// newTurnHooks registers the callbacks and event consumers of one turn of
// agent on opts, whose model is model. The order is behaviour, and
// TestTurnHooksOrder pins it:
//
//   - TurnFinished: the stream is flushed, then the eviction notice posted,
//     then the progress heartbeat stopped (so no trailing partial text is
//     lost and no placeholder edit lands after the reply);
//   - ModelCall: the "LLM finish" line, then the turn's usage, then the
//     dumps, then the reasoning (as each provider call ends);
//   - AfterModel: generated images are saved and delivered before the
//     reasoning can stand in for an empty reply, and both run before the
//     engine stores the assistant message (the notes are in it);
//   - ToolCallFinished: the tool's statistics, then its audit row, after
//     the call (the audit row has the call's outcome and duration; nothing
//     is audited before a call runs);
//   - redaction applies to logged and rendered error text only, as before;
//     tool output is stored as the tool returned it.
func (al *AgentLoop) newTurnHooks(ctx context.Context, agent *AgentInstance, opts processOptions, model string) *turnHooks {
	h := &turnHooks{log: logger.SDK()}
	st := &turnState{}

	// Partial-text streaming: a channel that opts in gets the model's text as
	// it is generated, coalesced into sentence/length chunks. One forwarder
	// serves every model call in the turn.
	if streamToolNarration && al.channelManager != nil && al.channelManager.SupportsStreaming(opts.Channel) {
		channel, chatID := opts.Channel, opts.ChatID
		st.stream = newStreamCoalescer(func(batch string) {
			al.channelManager.StreamDelta(ctx, channel, chatID, batch)
		})
		h.add(hook.Plugin{
			Name:        "stream",
			OnTextDelta: func(_ context.Context, ev hook.TextDelta) { st.stream.Add(ev.Text) },
			// Any buffered remainder that never hit a boundary is flushed
			// before the turn's reply is published.
			OnEvent: func(_ context.Context, ev hook.Event) {
				if _, ok := ev.(hook.TurnFinished); ok {
					st.stream.Flush()
				}
			},
		})
	}

	h.add(hook.Plugin{Name: "llm-finish", OnEvent: func(ctx context.Context, ev hook.Event) {
		if c, ok := ev.(hook.ModelCall); ok {
			al.emitFinish(ctx, agent, opts, c)
		}
	}})
	h.add(hook.Plugin{Name: "usage", OnEvent: func(_ context.Context, ev hook.Event) {
		if c, ok := ev.(hook.ModelCall); ok && c.Err == nil && c.Response != nil {
			addTurnUsage(opts.UsageOut, c.Response, c.Model.Provider, model)
		}
	}})
	h.add(hook.Plugin{Name: "dumps", OnEvent: func(_ context.Context, ev hook.Event) {
		if c, ok := ev.(hook.ModelCall); ok && c.Err == nil && c.Response != nil {
			al.dumpResponse(agent, opts, model, c)
		}
	}})
	// The reasoning is published on its own goroutine, which may outlive the
	// model loop: it gets the context the turn was started with, as it
	// always has, not the loop's.
	h.add(hook.Plugin{Name: "reasoning", OnEvent: func(_ context.Context, ev hook.Event) {
		if c, ok := ev.(hook.ModelCall); ok && c.Err == nil && c.Response != nil {
			al.deliverReasoning(ctx, agent, opts, c.Response.Reasoning)
		}
	}})

	if notify := al.fallbackNotifier(ctx, opts); notify != nil {
		h.add(hook.Plugin{Name: "fallback-notice", OnFallback: func(_ context.Context, ev hook.Fallback) {
			notify(fallbackAttempts(ev.Passed), fallbackCandidate(ev.Next))
		}})
	}
	h.add(hook.Plugin{Name: "compaction-notice", OnCompaction: func(ctx context.Context, ev hook.Compaction) {
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
	}})

	images := &turnImages{al: al, agent: agent, opts: opts, model: model}
	h.add(hook.Plugin{Name: "images", AfterModel: images.afterModel})
	h.add(hook.Plugin{Name: "reasoning-as-content", AfterModel: al.reasoningAsContent})

	// Breadcrumbs go to a user chat, never the internal "system" channel.
	if opts.Channel != "" && opts.Channel != "system" && opts.ChatID != "" && al.getShowToolActivity(agent, opts.SessionKey) {
		h.add(hook.Plugin{Name: "breadcrumbs", OnEvent: func(ctx context.Context, ev hook.Event) {
			if s, ok := ev.(hook.ToolCallStarted); ok {
				al.publishBreadcrumb(ctx, opts, hookToolCall(s.Call))
			}
		}})
	}
	h.add(hook.Plugin{Name: "tool-record", OnEvent: func(ctx context.Context, ev hook.Event) {
		if f, ok := ev.(hook.ToolCallFinished); ok {
			recordToolCall(ctx, agent, opts, f)
		}
	}})
	h.add(hook.Plugin{Name: "evictions", OnEvent: func(ctx context.Context, ev hook.Event) {
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
			st.mu.Lock()
			evicted := st.evicted
			st.mu.Unlock()
			al.publishEvictionNotice(ctx, agent, opts, evicted)
		}
	}})
	// Progress heartbeat: keep a long turn visibly alive by editing its
	// placeholder every progress_interval with a running tool-call count.
	h.add(hook.Plugin{Name: "progress", OnEvent: func(ctx context.Context, ev hook.Event) {
		switch ev.(type) {
		case hook.TurnStarted:
			st.stopProgress = al.startProgressUpdates(ctx, opts.Channel, opts.ChatID,
				al.GetConfig().Agents.Defaults.GetProgressInterval(), &st.completedTools)
		case hook.TurnFinished:
			if st.stopProgress != nil {
				st.stopProgress()
			}
		}
	}})
	h.addInterim(interimHooks{
		Name:          "progress",
		ToolsReturned: func(_ context.Context, n int) { st.completedTools.Add(int64(n)) },
	})
	h.addInterim(interimHooks{
		Name:    "narration",
		Narrate: func(ctx context.Context, _ int, content string) { al.publishNarration(ctx, opts, content) },
	})
	h.addInterim(interimHooks{
		Name: "tool-output",
		ToolsReturned: func(context.Context, int) {
			st.streamActivity = al.GetConfig().Agents.Defaults.StreamToolActivity
		},
		ToolResult: func(ctx context.Context, call hook.ToolCall, result *toolkit.Result) {
			// Each tool's own output reaches the user only with
			// stream_tool_activity; otherwise the user gets just the final
			// answer.
			if st.streamActivity && !result.Silent && result.ForUser != "" {
				al.publishToolForUser(ctx, opts, call.Name, result)
			}
			if len(result.Media) > 0 {
				al.publishToolMedia(ctx, opts, call.Name, result)
			}
		},
	})
	h.addInterim(interimHooks{
		Name: "vision-describe",
		DescribeImages: func(ctx context.Context, images, focus []string) (string, bool, bool) {
			return al.describeToolImages(ctx, agent, opts, images, focus)
		},
	})
	return h
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

// runLLMIteration executes the LLM call loop with tool handling and returns
// how it ended: the turn engine, with this turn's callbacks and consumers.
func (al *AgentLoop) runLLMIteration(
	ctx context.Context,
	agent *AgentInstance,
	messages []providers.Message,
	opts processOptions,
	cm ctxengine.ContextManager,
	mem *cogmem.Session,
) (modelReply, error) {
	// Provider error logs name the agent without correlating timestamps.
	ctx = providers.WithAgentID(ctx, agent.ID)
	turn, ctx := al.newHostEngineTurn(ctx, agent, messages, opts, cm, mem)
	res, err := turn.run(ctx)
	if err != nil {
		return modelReply{iterations: res.iterations}, err
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
		imageNotes:   res.notes,
	}, nil
}

// newHostEngineTurn is the engine turn of agent on opts over messages, with
// the turn's model choice (made now when opts has none) and its callbacks
// (registered and put on the returned context now when opts has none).
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
	if opts.turnHooks == nil {
		opts.turnHooks = al.newTurnHooks(ctx, agent, opts, tm.model)
		ctx = beginTurnContext(ctx, al.turnScope(ctx, agent, opts.SessionKey), opts.turnHooks, al.clk())
	}
	scope, _ := hook.ScopeOf(ctx)
	spec := engineSpec{
		agentID: agent.ID,
		scope:   scope,
		message: hook.Message{Text: opts.UserMessage, Media: opts.Media, Source: opts.SenderID},

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

		cm:    cm,
		mem:   mem,
		env:   &llmTurnEnv{al: al, agent: agent, opts: opts, mem: mem},
		hooks: opts.turnHooks,
	}
	return newEngineTurn(spec, messages), ctx
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
	opts  processOptions
	mem   *cogmem.Session
}

func (e *llmTurnEnv) toolDefs() []spawnllm.ToolDefinition {
	return e.agent.Tools.ToolDefs(nil).Send
}

// executeTool runs a call with the agent's allow checker on its context (so
// execution enforces the tool allowlist again, defense in depth), and with
// the turn's reply chat, so files a tool sends go where the reply goes. An
// AsyncExecutor tool's late result is routed by asyncToolCallback.
func (e *llmTurnEnv) executeTool(ctx context.Context, name string, args map[string]any) *toolkit.Result {
	asyncCallback := e.al.asyncToolCallback(ctx, e.agent, e.opts, name)
	execCtx := tools.WithToolAllowChecker(ctx, e.agent.Config)
	execCtx = tools.WithReplyChat(execCtx, tools.ReplyChat{Channel: e.opts.Channel, ChatID: e.opts.ChatID})
	return e.agent.Tools.ExecuteWithContext(execCtx, name, args, e.opts.Channel, e.opts.ChatID, asyncCallback)
}

func (e *llmTurnEnv) assembleRequest(ctx context.Context, defs []spawnllm.ToolDefinition, history []spawnllm.Message) ctxengine.AssembleRequest {
	return e.al.assembleRequestWithDefs(ctx, e.agent, e.mem, e.opts, defs, history)
}

func (e *llmTurnEnv) modelFacts(name string) engineModelFacts {
	return e.al.modelFactsFor(e.agent, name)
}

func (e *llmTurnEnv) compressionManager() (ctxengine.ContextManager, func()) {
	return e.al.getContextManager(e.agent, e.opts.SessionKey)
}

func (e *llmTurnEnv) storeFailed(what string, err error) error {
	return e.al.sessionStoreWriteFailed(e.agent, e.opts.SessionKey, what, err)
}

func (e *llmTurnEnv) sessionSummary() string {
	return e.agent.Sessions.GetSummary(e.opts.SessionKey)
}

func (e *llmTurnEnv) stopCause(ctx context.Context) (bool, bool) {
	return shuttingDown(ctx), stoppedOnPurpose(ctx)
}

// trackModelCall counts the call in flight, so a replaced client closes only
// after it (WaitTurns).
func (e *llmTurnEnv) trackModelCall() func() {
	e.al.activeRequests.Add(1)
	return e.al.activeRequests.Done
}

func (e *llmTurnEnv) directClient(name string) (spawnllm.LLMProvider, func(), error) {
	return e.al.catalog.Client(name)
}

func (e *llmTurnEnv) directProtocol(name string) (string, bool) {
	info, ok := e.al.catalog.Get(name)
	return info.Provider.Protocol, ok
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

// modelFactsFor is what the engine needs to know of model (a model_name) for
// a request of agent: its limits for the agent (agent may be nil), its
// vision mode (VisionOff for a model the configuration does not have) and
// whether its provider requires reasoning content.
func (al *AgentLoop) modelFactsFor(agent *AgentInstance, model string) engineModelFacts {
	f := engineModelFacts{vision: config.VisionOff}
	if agent != nil {
		l := agent.limitsFor(model)
		f.maxTokens, f.thinkingLevel = l.maxTokens, string(l.thinkingLevel)
	}
	cfg := al.GetConfig()
	mc, err := cfg.GetModelConfig(model)
	if err != nil || mc == nil {
		return f
	}
	f.vision = mc.Vision
	if mc.Provider != "" {
		if prov, perr := cfg.GetProvider(mc.Provider); perr == nil && prov != nil {
			f.requireReasoning = prov.RequireReasoningContent
		}
	}
	return f
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
func (al *AgentLoop) describeToolImages(ctx context.Context, agent *AgentInstance, opts processOptions, images, focus []string) (string, bool, bool) {
	if len(agent.VisionClients) == 0 {
		return "", false, false
	}
	focusParts := slices.Clone(focus)
	if lu := strings.TrimSpace(opts.UserMessage); lu != "" {
		focusParts = append(focusParts, "User's request: "+lu)
	}
	desc, ok := al.describeImages(ctx, agent, images, strings.Join(focusParts, "\n"))
	content := "An image was returned by a tool, but this model cannot view images and no description could be produced."
	if ok {
		content = "Image(s) described for you (this model cannot view images):\n" + desc
	}
	return content, ok, true
}
