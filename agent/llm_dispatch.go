// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// llmMaxRetries is how many times one dispatch is retried after a timeout or
// a context-window overflow.
const llmMaxRetries = 2

// timeoutRetryBackoff is the step of the backoff before retrying a timed-out
// dispatch: the n-th retry waits n steps, jittered.
const timeoutRetryBackoff = 5 * time.Second

// llmDispatch is one iteration's model request.
type llmDispatch struct {
	defs []providers.ToolDefinition
	// llmOpts are the request options every call shares; the per-model ones
	// (max_tokens, thinking_level) are added for each call by requestOptions.
	llmOpts map[string]any
	// maxTokensHalvings is how many times a context-window retry halved
	// max_tokens; each call halves its model's max_tokens that many times.
	maxTokensHalvings int
	// provider is the config provider name serving the dispatch, updated when
	// the fallback chain lands on another candidate.
	provider string
	// promptEstimate is the engine's prompt-token estimate for the request.
	promptEstimate int
}

// newDispatch builds the request for this iteration. The model that will
// serve it first decides which options apply: a CLI takes no HTTP request
// parameters.
func (t *llmTurn) newDispatch(defs []providers.ToolDefinition, promptEstimate int) *llmDispatch {
	agent := t.agent

	llmOpts := map[string]any{
		"prompt_cache_key": agent.ID,
	}
	provider, isCLI := "", false
	switch {
	case len(t.candidates) > 0:
		provider = t.candidates[0].Provider
		isCLI = config.CLIAgentByProtocol(t.candidates[0].Protocol) != nil
	case t.direct != "":
		if info, ok := t.al.catalog.Get(t.direct); ok {
			isCLI = config.CLIAgentByProtocol(info.Provider.Protocol) != nil
		}
	}
	// CLI providers run a subprocess and take no HTTP request parameters.
	// With no temperature configured none is sent: the model's default applies.
	if !isCLI && agent.Temperature != nil {
		llmOpts["temperature"] = *agent.Temperature
	}
	// Providers that cannot stream text deltas (CLI, Anthropic) ignore this.
	if t.stream != nil {
		llmOpts[providers.TextDeltaOption] = t.stream.Add
	}

	return &llmDispatch{
		defs:           defs,
		llmOpts:        llmOpts,
		provider:       provider,
		promptEstimate: promptEstimate,
	}
}

// requestModel sends the dispatch, retrying a timeout after a backoff and a
// context-window overflow after compressing, up to llmMaxRetries times. An
// error it returns ends the turn.
func (t *llmTurn) requestModel(ctx context.Context, d *llmDispatch) (*providers.LLMResponse, error) {
	var err error
	for retry := 0; retry <= llmMaxRetries; retry++ {
		logger.InfoCF("agent", "LLM dispatch", map[string]any{
			"agent_id":     t.agent.ID,
			"iteration":    t.iteration,
			"provider":     d.provider,
			"model":        t.model,
			"num_messages": len(t.messages),
			"num_tools":    len(d.defs),
			"max_tokens":   d.maxTokens(t.limits.maxTokens),
		})
		var response *providers.LLMResponse
		response, err = t.callModel(ctx, d)
		if err == nil {
			addTurnUsage(t.opts.UsageOut, response, d.provider, t.model)
			if d.promptEstimate > 0 && response != nil && response.Usage != nil && response.Usage.PromptTokens > 0 {
				t.cm.ObserveUsage(d.promptEstimate, response.Usage.PromptTokens)
			}
			return response, nil
		}

		isTimeoutError, isContextError := classifyLLMError(err, d.provider, t.model)
		if isTimeoutError && retry < llmMaxRetries {
			if waitErr := waitToRetry(ctx, t.al.clk(), err, retry); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if isContextError && retry < llmMaxRetries {
			t.compressForRetry(ctx, d, err, retry)
			continue
		}
		break
	}
	return nil, t.dispatchFailed(ctx, err)
}

// emitFinish writes the "LLM finish" line for one provider call: provider and
// ourModel name the config provider and model_name that were called,
// requestedModel the model id sent to it.
func (t *llmTurn) emitFinish(
	ctx context.Context,
	provider, ourModel, requestedModel string,
	elapsed time.Duration,
	resp *providers.LLMResponse,
	err error,
) {
	kind := llmKindAgent
	if t.agent.Spec.IsClone() {
		kind = llmKindSubagent
	}
	emitLLMFinishEvent(llmFinish{
		caller:         callerOf(t.agent),
		kind:           kind,
		iteration:      t.iteration,
		ourProvider:    provider,
		ourModel:       ourModel,
		requestedModel: requestedModel,
		sessionKey:     t.opts.SessionKey,
		channel:        t.opts.Channel,
		chatID:         t.opts.ChatID,
		turnID:         turnIDFrom(ctx),
		elapsedMs:      elapsed.Milliseconds(),
		resp:           resp,
		err:            err,
	})
}

// waitToRetry waits out the backoff, on clk, before retrying a timed-out
// dispatch. It returns the context's error when the turn's budget ran out
// first; such a turn is not retried, so no retry is logged.
func waitToRetry(ctx context.Context, clk clock.Clock, err error, retry int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	backoff := withJitter(time.Duration(retry+1) * timeoutRetryBackoff)
	logger.WarnCF("agent", "Timeout error, retrying after backoff", map[string]any{
		"error":   redactedError(err),
		"retry":   retry,
		"backoff": backoff.String(),
	})
	timer := clk.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

// compressForRetry answers a context-window error: the user is told once,
// the session is force-compressed and the request reassembled. When that
// does not shrink the history, max_tokens is halved instead, since the
// provider's per-request limit may be below the model's stated maximum
// (e.g. Groq's free tier).
func (t *llmTurn) compressForRetry(ctx context.Context, d *llmDispatch, err error, retry int) {
	logger.WarnCF(
		"agent",
		"Context window error detected, attempting compression",
		map[string]any{
			"error": redactedError(err),
			"retry": retry,
		},
	)

	if retry == 0 && !constants.IsInternalChannel(t.opts.Channel) {
		if pubErr := t.al.bus.PublishOutbound(ctx, bus.OutboundMessage{
			Channel: t.opts.Channel,
			ChatID:  t.opts.ChatID,
			Content: "Context window exceeded. Compressing history and retrying...",
		}); pubErr != nil {
			logger.WarnCF("agent", "Failed to publish compression notice",
				map[string]any{"error": pubErr.Error(), "channel": t.opts.Channel})
		}
	}

	prevMsgCount := len(t.messages)
	comprMgr, releaseComprMgr := t.al.getContextManager(t.agent, t.opts.SessionKey) //nolint:contextcheck // compaction reporter: ctxengine's callback has no context, so it publishes on its own
	t.releases = append(t.releases, releaseComprMgr)
	if ferr := comprMgr.ForceCompress(ctx); ferr != nil {
		logger.WarnCF("agent", "force compression failed",
			map[string]any{"error": ferr.Error(), "session": t.opts.SessionKey})
	}
	if asm, berr := comprMgr.Assemble(ctx, t.al.assembleRequestWithDefs(ctx, t.agent, t.mem, t.opts, d.defs, t.messages)); berr == nil {
		t.messages = asm.Messages
		d.promptEstimate = asm.PromptTokenEstimate
	}

	if len(t.messages) < prevMsgCount {
		return
	}
	// Counted on every such retry: each model's max_tokens is halved by
	// d.maxTokens down to its own floor, so a selected model already at the
	// floor still lets a larger fallback be lowered.
	current := d.maxTokens(t.limits.maxTokens)
	d.maxTokensHalvings++
	logger.WarnCF("agent", "History already minimal; reducing max_tokens for retry",
		map[string]any{
			"agent_id":       t.agent.ID,
			"model":          t.model,
			"old_max_tokens": current,
			"new_max_tokens": d.maxTokens(t.limits.maxTokens),
		})
}

// dispatchFailed is the error a dispatch that failed for good ends the turn
// with: an interruption by shutdown, a deliberate stop (/cancel, or an asker
// that stopped waiting), or a failure.
func (t *llmTurn) dispatchFailed(ctx context.Context, err error) error {
	if shuttingDown(ctx) {
		return fmt.Errorf("LLM call interrupted: %w", context.Cause(ctx))
	}
	if stoppedOnPurpose(ctx) {
		logger.InfoCF("agent", "LLM call stopped: the turn was cancelled",
			map[string]any{
				"agent_id":  t.agent.ID,
				"iteration": t.iteration,
				"model":     t.model,
				"cause":     context.Cause(ctx).Error(),
			})
		return fmt.Errorf("LLM call stopped: %w: %w", context.Cause(ctx), err)
	}
	logger.ErrorCF("agent", "LLM call failed",
		map[string]any{
			"agent_id":  t.agent.ID,
			"iteration": t.iteration,
			"model":     t.model,
			"error":     redactedError(err),
		})
	return fmt.Errorf("LLM call failed after retries: %w", err)
}

// callDefaultModel makes the dispatch's one call for an agent none of whose
// models resolves: on the default model's client (t.direct), with the
// agent's first configured name as the model and the agent's own limits,
// writing one "LLM finish" line. Nothing is recorded against a model's
// health and there is no fallback.
func (t *llmTurn) callDefaultModel(ctx context.Context, d *llmDispatch, hasTools bool) (*providers.LLMResponse, error) {
	client, release, err := t.al.catalog.Client(t.direct)
	if err != nil {
		return nil, err
	}
	defer release()
	start := time.Now()
	t.sent = t.requestMessages(hasTools, isCLIClient(client))
	resp, err := client.Chat(ctx, t.agent.Identity(), t.al.messagesForModel(t.sent, t.model, hasTools), d.defs, t.model, t.requestOptions(d, hasTools, client, t.model))
	t.emitFinish(ctx, d.provider, t.model, t.model, time.Since(start), resp, err)
	return resp, err
}

// errNoModelConfigured ends a turn whose agent has no model to call. Its
// text is the one a configuration without models has always answered with.
var errNoModelConfigured = errors.New("no model configured — enable a model and add your API key in the configuration")

// callModel makes one call for the dispatch through the turn's pinned chain:
// each model in turn, skipping parked ones, until one answers. Every provider
// call it makes writes its own "LLM finish" line, and each candidate passed
// over is logged as the chain moves on.
func (t *llmTurn) callModel(ctx context.Context, d *llmDispatch) (*providers.LLMResponse, error) {
	al := t.al
	al.activeRequests.Add(1)
	defer al.activeRequests.Done()

	hasTools := len(d.defs) > 0
	if t.chain == nil || len(t.candidates) == 0 {
		if t.direct != "" {
			return t.callDefaultModel(ctx, d, hasTools)
		}
		t.sent = t.messages
		t.emitFinish(ctx, d.provider, t.model, t.model, 0, nil, errNoModelConfigured)
		return nil, errNoModelConfigured
	}
	fbResult, err := t.chain.Chat(ctx, t.agent.Identity(), models.Request{Tools: d.defs},
		// Image parts are dropped for a candidate whose model cannot take
		// them: a screenshot in history would otherwise fail every turn on
		// a non-vision candidate.
		models.WithPrepare(func(c models.Candidate, client providers.LLMProvider, _ models.Request) models.Request {
			t.sent = t.requestMessages(hasTools, isCLIClient(client))
			return models.Request{
				Messages: al.messagesForModel(t.sent, c.Name, hasTools),
				Tools:    d.defs,
				Options:  t.requestOptions(d, hasTools, client, c.Name),
			}
		}),
		models.WithNotify(t.loggingNotifier()),
		models.WithFinish(func(c models.Candidate, elapsed time.Duration, resp *providers.LLMResponse, err error) {
			t.emitFinish(ctx, c.Provider, c.Name, c.Model, elapsed, resp, err)
		}),
	)
	if err != nil {
		// Cooldown skips at the tail of the chain reach no notifier.
		if exhausted, ok := errors.AsType[*providers.FallbackExhaustedError](err); ok {
			t.logPassed(trailingSkips(exhausted.Attempts))
		}
		return nil, err
	}
	d.provider = fbResult.Candidate.Provider
	if len(fbResult.Attempts) > 0 {
		logger.InfoCF(
			"agent",
			fmt.Sprintf("Fallback: succeeded with %s/%s after %d attempts",
				fbResult.Candidate.Provider, fbResult.Candidate.Model, len(fbResult.Attempts)+1),
			map[string]any{"agent_id": t.agent.ID, "iteration": t.iteration},
		)
	}
	return fbResult.Response, nil
}

// requestOptions are the options for one provider call to model (a
// model_name), served by provider: the shared options plus that model's
// max_tokens (lowered by any context-window retry) and thinking level. On the
// turn's last step, where the low-steps notice tells the model not to call
// tools, the call also forbids tool calls (tool_choice none). The tools stay
// declared, so earlier tool calls in the history and the prompt cache stay
// valid. d.llmOpts is shared by every candidate the fallback chain tries, so
// it is copied rather than changed.
func (t *llmTurn) requestOptions(d *llmDispatch, hasTools bool, provider providers.LLMProvider, model string) map[string]any {
	limits := t.agent.limitsFor(model)
	opts := make(map[string]any, len(d.llmOpts)+3)
	maps.Copy(opts, d.llmOpts)
	opts["max_tokens"] = d.maxTokens(limits.maxTokens)
	// modelspec.ParseThinkingLevel guarantees ThinkingOff for empty/unknown values.
	if limits.thinkingLevel != ThinkingOff {
		if tc, ok := provider.(providers.ThinkingCapable); ok && tc.SupportsThinking() {
			opts["thinking_level"] = string(limits.thinkingLevel)
		} else {
			logger.WarnCF("agent", "thinking_level is set but current provider does not support it, ignoring",
				map[string]any{"agent_id": t.agent.ID, "model": model, "thinking_level": string(limits.thinkingLevel)})
		}
	}
	if !hasTools || t.iteration != t.agent.MaxIterations {
		return opts
	}
	if isCLIClient(provider) {
		return opts
	}
	opts[providers.ToolChoiceOption] = providers.ToolChoiceNone
	return opts
}

// isCLIClient reports whether the client runs a CLI subprocess.
func isCLIClient(p providers.LLMProvider) bool {
	c, ok := p.(providers.CLIProvider)
	return ok && c.IsCLI()
}

// minHalvedMaxTokens is the max_tokens at or below which a context-window
// retry stops halving it.
const minHalvedMaxTokens = 512

// maxTokens is the max_tokens a call to a model whose own is base sends:
// base halved once per context-window retry that lowered it, never from a
// value at or below minHalvedMaxTokens.
func (d *llmDispatch) maxTokens(base int) int {
	for range d.maxTokensHalvings {
		if base <= minHalvedMaxTokens {
			break
		}
		base /= 2
	}
	return base
}

// loggingNotifier wraps the turn's fallback notifier so each candidate the
// chain passes over is logged as it moves on: a failed one (the chain calls
// the notifier only when it falls back to another candidate, not for a
// same-model retry nor for a failure that ends the chain) and each cooldown
// skip.
func (t *llmTurn) loggingNotifier() providers.FallbackNotify {
	return func(passed []providers.FallbackAttempt, next providers.FallbackCandidate) {
		t.logPassed(passed)
		if t.notifier != nil {
			t.notifier(passed, next)
		}
	}
}

// trailingSkips returns the cooldown skips after the last candidate called,
// which no notifier saw.
func trailingSkips(attempts []providers.FallbackAttempt) []providers.FallbackAttempt {
	i := len(attempts)
	for i > 0 && attempts[i-1].Skipped {
		i--
	}
	return attempts[i:]
}

// logPassed logs each attempt the chain passed over: a failed candidate or a
// cooldown skip.
func (t *llmTurn) logPassed(attempts []providers.FallbackAttempt) {
	for _, attempt := range attempts {
		if !attempt.Skipped {
			logger.WarnCF("agent", "Fallback: candidate failed",
				map[string]any{
					"agent_id": t.agent.ID,
					"provider": attempt.Provider,
					"model":    attempt.Model,
					"reason":   attempt.Reason,
					"duration": attempt.Duration.Round(time.Millisecond),
					"error":    redactedError(attempt.Err),
				})
			continue
		}
		logger.WarnCF("agent", "Fallback: skipped candidate (cooldown)",
			map[string]any{
				"agent_id":  t.agent.ID,
				"provider":  attempt.Provider,
				"model":     attempt.Model,
				"reason":    attempt.Reason,
				"remaining": attempt.Remaining.Round(time.Second),
				"error":     redactedError(attempt.Err),
			})
	}
}

// turnModel is a turn's model choice, made once when the turn starts so its
// assembly, tools, context window and requests all follow the same model:
// the chain pinned for the turn (the active model first), its candidates,
// the first one's model_name, and its limits.
type turnModel struct {
	chain      *models.Chain
	candidates []providers.FallbackCandidate
	model      string
	limits     runLimits
	// direct is the model whose client serves an agent none of whose own
	// models resolves (defaultModelClient); "" otherwise.
	direct string
}

// selectTurnModel makes the model choice for a turn of agent on sessionKey:
// the agent's chat models pinned for the turn, the session's /model choice
// first and the rest in their order, so a
// multi-step tool chain never switches model mid-way. A fresh temporary
// agent's CLI models run isolated in its own workspace.
func (al *AgentLoop) selectTurnModel(agent *AgentInstance, sessionKey string) *turnModel {
	set := al.catalog.Set(agent.ID)
	scope := models.Scope(sessionKey)
	chain := set.Begin(models.Chat, scope)
	if agent.Spec.Fresh {
		chain = chain.Isolated(agent.Workspace)
	}
	candidates := chain.Candidates()
	var model, direct string
	if len(candidates) > 0 {
		model = candidates[0].Name
	} else {
		// The name status and /model show too (activeModelName).
		model = al.activeModelName(agent, sessionKey)
		direct = al.defaultModelClient()
	}
	return &turnModel{chain: chain, candidates: candidates, model: model, limits: agent.limitsFor(model), direct: direct}
}

// defaultModelClient is the model an agent none of whose models resolves
// runs on: the agents' default model (agents.defaults.models[0], by
// model_name), when the catalogue has it. Its client is called with the
// agent's first configured name as the model, with no fallback and no
// health kept, as the agent's own provider always served such an agent.
// "" when there is none: the turn fails as a configuration without models.
func (al *AgentLoop) defaultModelClient() string {
	cfg := al.GetConfig()
	if cfg == nil {
		return ""
	}
	mc, err := cfg.GetModelConfig(cfg.Agents.Defaults.DefaultModelName())
	if err != nil || mc == nil {
		return ""
	}
	if _, ok := al.catalog.Get(mc.ModelName); !ok {
		return ""
	}
	return mc.ModelName
}

// reasoningPlaceholder is what backfillReasoningContent writes into an empty
// reasoning_content. A single space, deliberately: DeepSeek V4 Pro rejects the
// empty string as "not passed back", so the value must be non-empty, and
// anything longer would be fabricated reasoning the model never produced.
const reasoningPlaceholder = " "

// backfillReasoningContent gives every assistant message a non-empty
// reasoning_content when the target provider demands one.
//
// DeepSeek V4 thinking mode requires the field on every assistant message in
// history whenever the request carries tools — including turns that made no tool
// call — and answers 400 without it. Messages written by a model that records no
// reasoning (any CLI provider, or the same model with thinking off) therefore
// wedge a session the moment it is pointed at DeepSeek: the same history replays
// every turn and is rejected every turn, and the fallback chain cannot rescue it
// because a 400 is not retriable.
//
// Three conditions, all necessary:
//   - the provider opts in via require_reasoning_content;
//   - the request carries tools, since without them DeepSeek ignores the field
//     entirely and there is nothing to satisfy;
//   - the message is an assistant turn whose reasoning_content is empty. Real
//     reasoning is never touched, so a thinking model's own output round-trips
//     unchanged.
//
// The input slice is not modified: callers share `messages` across fallback
// candidates, and one candidate's wire quirk must not follow the history to the
// next.
func (al *AgentLoop) backfillReasoningContent(
	messages []providers.Message,
	mc *config.ModelConfig,
	mcErr error,
	hasTools bool,
) []providers.Message {
	if !hasTools || mcErr != nil || mc == nil || mc.Provider == "" {
		return messages
	}
	prov, err := al.GetConfig().GetProvider(mc.Provider)
	if err != nil || prov == nil || !prov.RequireReasoningContent {
		return messages
	}

	// Copy lazily: most turns need no backfill at all once a thinking model has
	// been running, and copying the whole slice per dispatch is wasted work.
	out := messages
	copied := false
	for i := range messages {
		if messages[i].Role != "assistant" || messages[i].ReasoningContent != "" {
			continue
		}
		if !copied {
			out = make([]providers.Message, len(messages))
			copy(out, messages)
			copied = true
		}
		out[i].ReasoningContent = reasoningPlaceholder
	}
	return out
}

// messagesForModel returns messages suitable for the named model (keyed by
// alias / model_name). Vision-capable models get the slice unchanged; models
// with vision off or unset get a shallow copy with image Media removed. Some
// providers (e.g. deepseek via OpenRouter) reject the ENTIRE request with a 404
// when an image part is present, so a screenshot left in conversation history
// must not be replayed to a non-vision model. The persisted originals are never
// mutated, so a later turn on a vision model still surfaces the image. Unknown
// model is treated as no-vision, matching the tool-image injection default.
func (al *AgentLoop) messagesForModel(messages []providers.Message, modelKey string, hasTools bool) []providers.Message {
	mc, mcErr := al.GetConfig().GetModelConfig(modelKey)

	// Reasoning backfill runs first and independently of the media rules below:
	// it is a wire-contract requirement, not a capability downgrade.
	messages = al.backfillReasoningContent(messages, mc, mcErr, hasTools)

	if mcErr == nil && mc != nil {
		if mc.Vision == config.VisionUserMessage || mc.Vision == config.VisionToolResponse {
			return messages
		}
	}
	hasMedia := false
	for i := range messages {
		if len(messages[i].Media) > 0 {
			hasMedia = true
			break
		}
	}
	if !hasMedia {
		return messages
	}
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if len(out[i].Media) == 0 {
			continue
		}
		n := len(out[i].Media)
		out[i].Media = nil
		// Say so instead of stripping silently: the model should know the message
		// carried attachments it cannot see (any media:// refs remain in the text
		// via the attachment marker and can be delegated via agent_spawn).
		out[i].Content = strings.TrimRight(out[i].Content, "\n") +
			fmt.Sprintf("\n[%d attachment(s) on this message are hidden — the current model cannot view images]", n)
	}
	return out
}

// backoffJitterFraction spreads a retry backoff by ±20% so concurrent turns
// retrying the same provider do not land on it in lockstep.
const backoffJitterFraction = 0.2

// withJitter returns d scaled by a random factor in [1-jitter, 1+jitter].
func withJitter(d time.Duration) time.Duration {
	f := 1 + backoffJitterFraction*(2*rand.Float64()-1) //nolint:gosec // retry spread, not security-relevant
	return time.Duration(float64(d) * f)
}

// localContextLimitPatterns are context-window/token-limit markers that
// spawnllm's ClassifyError does not recognise. Candidates for upstreaming;
// remove each one here once spawnllm classifies it.
var localContextLimitPatterns = []string{
	"context window",
	"token limit",
	"max_tokens",
	"invalidparameter",
}

// classifyLLMError sorts a dispatch error into the two retry buckets the turn
// acts on: a transient timeout (retry after backoff) or a context-window
// overflow (compress and retry). spawnllm's classifier is the source of
// truth; the local patterns cover only what it does not classify.
func classifyLLMError(err error, provider, model string) (isTimeout, isContext bool) {
	cls := providers.ClassifyError(err, provider, model)
	if errors.Is(err, context.DeadlineExceeded) || (cls != nil && cls.Reason == providers.FailoverTimeout) {
		return true, false
	}
	if cls != nil && cls.Reason == providers.FailoverContextLimit {
		return false, true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range localContextLimitPatterns {
		if strings.Contains(msg, p) {
			return false, true
		}
	}
	var exhausted *providers.FallbackExhaustedError
	if errors.As(err, &exhausted) && exhausted.AllContextLimit() {
		return false, true
	}
	return false, false
}
