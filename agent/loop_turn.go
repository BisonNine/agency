// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm/common"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
	"go.bison9.com/agency/utils"
)

// processOptions configures how a message is processed
type processOptions struct {
	SessionKey      string            // Session identifier for history/context
	Channel         string            // Target channel for tool execution
	ChatID          string            // Target chat ID for tool execution
	UserMessage     string            // User message content (may include prefix)
	Media           []string          // media:// refs from inbound message
	DefaultResponse string            // Response when LLM returns empty
	SendResponse    bool              // Whether to send response via bus
	IsRetry         bool              // True when message is a /retry retrigger (skip AddMessage)
	ResetSession    bool              // True when this message is a session_clear handoff: reset before handling
	SenderID        string            // Originating sender identifier for source attribution
	SenderName      string            // Human-readable sender label (display name + canonical ID)
	IsGroup         bool              // True when the inbound message came from a group/multi-listener chat
	IterationsOut   *int              // optional: runAgentLoop writes the LLM iteration count here
	UsageOut        *global.TurnUsage // optional: every successful LLM call's accounting is added here
	// OutcomeOut, when set, receives bus.OutcomeEmpty when the model produced
	// no reply (whatever fallback text is returned) or bus.OutcomeError for an
	// abnormal empty termination. Left untouched for a normal reply, and on an
	// error return (the caller decides that outcome from the error).
	OutcomeOut *string
	// MessageID and ReplyRequired describe the inbound message the turn
	// answers; they are recorded with the pending turn so a restart's replay
	// still owes the sender its reply.
	MessageID     string
	ReplyRequired bool
	// FromPerson marks a turn answering a message someone wrote in a chat
	// (fromPerson), the only kind told when a compaction makes it wait.
	FromPerson bool
	// OnReplyDelivery, when set with SendResponse, is told how the delivery
	// of the reply ended (bus.OutboundMessage.OnDelivery), with the reply.
	// It must not block.
	OnReplyDelivery func(reply string, err error)
	// turnModel is the turn's model choice, made by runAgentLoop when the
	// turn starts; nil when the turn engine is entered without one, which
	// then makes it.
	turnModel *turnModel
}

// turnLimits are the limits of the turn's model: the choice made when the
// turn started, else the session's active model.
func (al *AgentLoop) turnLimits(agent *AgentInstance, opts processOptions) runLimits {
	if opts.turnModel != nil {
		return opts.turnModel.limits
	}
	return al.sessionLimits(agent, opts.SessionKey)
}

// setOutcome records how the turn ended in opts.OutcomeOut, when requested.
func (opts processOptions) setOutcome(outcome string) {
	if opts.OutcomeOut != nil {
		*opts.OutcomeOut = outcome
	}
}

// runAgentLoop is the core message processing logic: one turn of agent on
// opts, from saving the user's message to the reply, run by the turn engine.
func (al *AgentLoop) runAgentLoop(
	ctx context.Context,
	agent *AgentInstance,
	opts processOptions,
) (string, error) {
	// Every provider call of the turn names the agent in its error logs,
	// including the compression paths that fan out from the turn.
	ctx = providers.WithAgentID(ctx, agent.ID)

	agent, endTurn, err := al.beginAgentTurn(agent, opts.SessionKey)
	if err != nil {
		return "", err
	}
	defer endTurn()

	// A human agent's turn is answered by the person, never by a model.
	if agent.HumanModel != "" {
		return al.runHumanEngineTurn(ctx, agent, opts)
	}

	// A single-shot agent keeps nothing between turns: its conversation is
	// discarded once the turn is over (after the context manager is released
	// below), however the turn ended. That includes a turn shutdown
	// interrupted: restart recovery replays config agents' turns only, never a
	// temporary agent's, so nothing would ever read it again.
	if agent.Spec.SingleShot() {
		defer al.discardConversation(context.WithoutCancel(ctx), agent, opts.SessionKey)
	}

	// The context manager and the cognitive-memory session (nil for an agent
	// without it) of this session.
	cm, mem, releaseCtxMgr := al.getSessionContext(agent, opts.SessionKey) //nolint:contextcheck // compaction reporter: ctxengine's callback has no context, so it publishes on its own
	defer releaseCtxMgr()

	// The turn's model is chosen once, and the session's history is trimmed
	// and compacted to its context window: /model may have changed it since
	// the manager was built (a no-op when it has not).
	opts.turnModel = al.selectTurnModel(agent, opts.SessionKey)
	if werr := cm.SetContextWindow(opts.turnModel.limits.contextWindow); werr != nil {
		logger.WarnCF("agent", "Context window not set for the turn's model",
			map[string]any{"agent_id": agent.ID, "model": opts.turnModel.model, "error": werr.Error()})
	}

	// An age-only compaction the engine deferred runs once the reply is out,
	// however the turn ends.
	afterReply := claimAfterReply(ctx)
	defer al.compactAfterReply(afterReply, agent, opts.SessionKey) //nolint:contextcheck // it outlives the turn, so it runs on the loop's run context, not the turn's

	// After the session context, so the session's token exists.
	ctx, endTokenTurn := al.scopeSessionToken(ctx, agent, opts)
	defer endTokenTurn()
	ctx = withCompactionNotice(ctx, agent, opts)

	return al.runModelTurn(ctx, agent, opts, cm, mem)
}

// runHumanEngineTurn runs a human agent's turn through the turn engine: the
// person answers it (BeforeTurn), nothing is stored.
func (al *AgentLoop) runHumanEngineTurn(ctx context.Context, agent *AgentInstance, opts processOptions) (string, error) {
	hooks := &turnHooks{log: logger.SDK()}
	hooks.add(hook.Plugin{Name: "person", BeforeTurn: func(ctx context.Context, _ *hook.Turn) (*hook.TurnResult, error) {
		reply, err := al.runHumanTurn(ctx, agent, opts)
		if err != nil {
			return nil, err
		}
		return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: reply}, nil
	}})
	ctx = beginTurnContext(ctx, al.turnScope(ctx, agent, opts.SessionKey), hooks, al.clk())
	scope, _ := hook.ScopeOf(ctx)
	turn := newEngineTurn(engineSpec{
		agentID: agent.ID,
		scope:   scope,
		message: turnMessage(opts),
		options: hook.RunOptions{AlreadyStored: opts.IsRetry, Depth: scope.Depth},
		clock:   al.clk(),
		log:     logger.SDK(),
		env:     &llmTurnEnv{al: al, agent: agent, opts: &opts},
		hooks:   hooks,
	}, nil)
	_, err := turn.run(ctx)
	return al.turnOutcome(turn, err)
}

// beginAgentTurn marks agent in a turn: a temporary agent is never deleted
// mid-turn, its idle time counts from the end of its last turn, and an
// instance a reload replaced is closed only once no turn runs on it. An
// instance resolved before a reload replaced it is closed (or about to be),
// so the turn runs on the current instance instead, and is dropped with
// errAgentGone only when the agent is gone. It returns the instance the turn
// runs on and the function that ends the turn.
func (al *AgentLoop) beginAgentTurn(agent *AgentInstance, sessionKey string) (*AgentInstance, func(), error) {
	registry := al.GetRegistry()
	if registry == nil {
		return agent, func() {}, nil
	}
	endTurn, current := registry.BeginTurn(agent.ID, agent)
	if current {
		return agent, endTurn, nil
	}
	if fresh, ok := registry.Get(agent.ID); ok {
		agent = fresh
		endTurn, current = registry.BeginTurn(agent.ID, agent)
	}
	if !current {
		logger.WarnCF("agent", "Turn dropped: the agent no longer exists",
			map[string]any{"agent_id": agent.ID, "agent": agent.Label(), "session_key": sessionKey})
		return agent, nil, errAgentGone
	}
	return agent, endTurn, nil
}

// scopeSessionToken records the turn on the session's token, so a CLI
// provider's MCP tool calls, which bypass the agent loop, act within it: the
// turn's sub-agent depth (every turn, so a later ordinary turn resets it),
// the agents waiting on the turn (none of which may be asked from it; see
// Ask), and the chat to send user-facing output to. It returns ctx with the
// running agent added to the ask chain, and the function that ends the
// turn on the token.
func (al *AgentLoop) scopeSessionToken(ctx context.Context, agent *AgentInstance, opts processOptions) (context.Context, func()) {
	al.mu.RLock()
	sti := al.sessionTokenIssuer
	al.mu.RUnlock()
	if sti != nil {
		sti.SetDepth(opts.SessionKey, toolsagents.SpawnDepth(ctx))
	}
	ctx = tools.WithAskChainAgent(ctx, agent.ID)
	if sti == nil {
		return ctx, func() {}
	}
	sti.SetTurnScope(opts.SessionKey, tools.AskChain(ctx))
	// The turn's MCP output (msg_send, msg_send_file, ForUser, breadcrumbs,
	// background results) goes to the chat this turn replies to, recorded
	// per turn, never to the chat the session last heard from.
	return ctx, sti.BeginTurn(opts.SessionKey, opts.Channel, opts.ChatID)
}

// prepareSession readies the session for the turn's message, adjusting
// *opts and turn.Message: a session_clear handoff resets the session, then
// releases its pins and reissues its token (the host's order, so a reset
// that panics leaves both as they were; the run asks the engine for no
// reset), a
// single-shot turn starts blank, whispers held for the agent open its
// message, and the inbound media are described and pinned.
func (al *AgentLoop) prepareSession(ctx context.Context, agent *AgentInstance, cm ctxengine.ContextManager, opts *processOptions, turn *hook.Turn) error {
	// session_clear publishes an inbound tagged metaSessionReset. The reset
	// happens at a clean turn boundary, before the notice is saved, so this
	// turn starts on a clean (archive-preserved) context; the reissued
	// session token reaches the model in this turn's system prompt.
	if opts.ResetSession {
		if err := cm.Reset(ctx); err != nil {
			logger.WarnCF("agent", "session_clear: Reset failed", map[string]any{
				"session_key": opts.SessionKey,
				"error":       err.Error(),
			})
		}
		// The cleared history no longer references any media; let its files age out.
		al.releaseSessionPins(opts.SessionKey)
		al.reissueSessionToken(agent, opts.SessionKey)
	}

	// Whatever a previous single-shot turn left behind (its discard skipped
	// because the session was in use) is cleared, and a retried message is
	// saved again as the turn's only message (AlreadyStored is off).
	if agent.Spec.SingleShot() {
		if err := cm.Reset(ctx); err != nil {
			return fmt.Errorf("single-shot reset: %w", err)
		}
		opts.IsRetry = false
	}

	// Whispers open the agent's next message, once. A retried message is
	// already in history, so they wait for the next one.
	if !opts.IsRetry {
		opts.UserMessage = al.prependWhispers(agent, opts.UserMessage)
		turn.Message.Text = opts.UserMessage
	}
	if len(opts.Media) > 0 && !turn.Options.AlreadyStored {
		userMsg := providers.Message{Role: "user", Content: turn.Message.Text}
		al.attachInboundMedia(ctx, agent, *opts, &userMsg)
		turn.Message.Text, turn.Message.Media = userMsg.Content, userMsg.Media
	}
	return nil
}

// attachInboundMedia puts the inbound message's media on userMsg.
func (al *AgentLoop) attachInboundMedia(ctx context.Context, agent *AgentInstance, opts processOptions, userMsg *providers.Message) {
	userMsg.Media = opts.Media
	// A text-only primary with a vision side-model configured gets the
	// image(s) described once here, the description folded into the text
	// (clearing Media), so every dispatch carries usable text instead of an
	// image the model cannot see. No-op for vision-capable primaries and when
	// vision is not configured.
	al.describeInboundMedia(ctx, agent, userMsg)
	// The media:// refs stay visible in the stored text and are pinned for
	// the session, so TTL cleanup does not reap files the history still
	// references. They are actionable: agent_spawn's media parameter accepts
	// them, so even a text-only primary can hand the original attachment to a
	// vision-capable worker.
	if refs := mediaRefsIn(opts.Media); len(refs) > 0 {
		al.pinSessionMediaRefs(opts.SessionKey, refs)
		userMsg.Content = appendMediaRefMarker(userMsg.Content, refs)
	}
}

// decideModelReply turns the model's reply into the turn's (AfterTurn):
// intentional silence and empty replies are handled, and what the engine
// stores, the chat is sent and whether its typing indicator is cleared
// (once the turn finished) are decided. A turn that failed, or that
// BeforeTurn answered, is left as it is.
func (al *AgentLoop) decideModelReply(agent *AgentInstance, opts processOptions, st *turnState, r *hook.TurnResult) {
	if r.Err != nil || r.Skipped {
		return
	}
	reply := modelReplyOf(*r, st)
	decided := turnReply{decided: true, outcome: hook.OutcomeOK, iterations: r.Iterations, images: r.Images}
	defer func() {
		r.Outcome = decided.outcome
		r.Reply = decided.content
		st.reply = decided
	}()
	// The no-response sentinel (e.g. a group message addressed to someone
	// else) is intentional silence: the typing indicator is cleared and
	// nothing is sent or stored, unlike a degenerate empty reply.
	if isNoResponseSentinel(reply.content) {
		logger.DebugCF("agent", "LLM declined to respond (no-response sentinel)",
			map[string]any{"agent_id": agent.ID, "session_key": opts.SessionKey})
		st.stopTyping = true
		opts.setOutcome(bus.OutcomeEmpty)
		decided = turnReply{outcome: hook.OutcomeEmpty}
		r.Stored = ""
		return
	}

	// The chat is shown every image of the turn that did not reach it; the
	// stored reply (r.Stored) notes only the last response's images, since
	// earlier ones are noted in the tool-call message that carried them.
	content := withImageNotes(reply.content, reply.imageReplies)
	// A last response of images alone that ended normally is a complete
	// reply; an empty, degenerate or cut-off one is not, whatever images
	// earlier responses of the turn produced.
	imagesOnly := reply.content == "" && reply.lastImages > 0 && reply.normal && !reply.degenerate
	if reply.content == "" && !imagesOnly {
		fallback, outcome := al.emptyReply(agent, opts, reply)
		opts.setOutcome(outcome)
		if fallback == "" {
			st.stopTyping = true
			decided = turnReply{outcome: hook.Outcome(outcome)}
			r.Stored = ""
			return
		}
		decided.outcome = hook.Outcome(outcome)
		content = withImageNotes(fallback, reply.imageReplies)
		decided.systemError = true
	}
	if content == "" {
		// Images alone, all sent to the chat: a complete reply with no text
		// to send.
		opts.setOutcome(bus.OutcomeOK)
		st.stopTyping = true
		decided.imagesOnly = true
		return
	}
	// A system error string is not the model's reply, so it is not stored;
	// the notes on the last response's images (r.Stored) still are.
	decided.content = content
}

// modelReplyOf is how a turn's model loop ended, from its result and the
// answers its consumers followed: the model's final content is the reply
// without the reply lines AfterModel added.
func modelReplyOf(r hook.TurnResult, st *turnState) modelReply {
	content := r.Reply
	if joined := strings.Join(st.replyLines, "\n"); joined != "" {
		if content == joined {
			content = ""
		} else {
			content = strings.TrimSuffix(content, "\n\n"+joined)
		}
	}
	return modelReply{
		content:      content,
		normal:       r.Normal,
		degenerate:   r.Degenerate,
		finishReason: r.FinishReason,
		iterations:   r.Iterations,
		images:       r.Images,
		lastImages:   st.lastImages,
		imageReplies: st.replyLines,
	}
}

// sendModelReply sends the reply AfterTurn decided once the turn finished
// with the outcome it was decided for (TurnFinished): to the chat when opts
// asks for it, then its "Response" line. Nothing is sent when storing it
// failed.
func (al *AgentLoop) sendModelReply(ctx context.Context, agent *AgentInstance, opts processOptions, st *turnState, f hook.TurnResult) {
	d := st.reply
	if !d.decided || f.Err != nil || f.Outcome != d.outcome {
		return
	}
	if d.imagesOnly {
		logger.InfoCF("agent", "Response",
			map[string]any{"agent_id": agent.ID, "session_key": opts.SessionKey, "iterations": d.iterations, "images": d.images})
		if opts.IterationsOut != nil {
			*opts.IterationsOut = d.iterations
		}
		return
	}
	if opts.SendResponse {
		al.publishTurnReply(ctx, opts, d.content)
	}

	// Content is logged only with log_message_content, for privacy.
	logMsg := "Response"
	if logger.GetLogMessageContent() {
		logMsg = "Response: " + utils.Truncate(d.content, 120)
	}
	logger.InfoCF("agent", logMsg,
		map[string]any{
			"agent_id":     agent.ID,
			"session_key":  opts.SessionKey,
			"iterations":   d.iterations,
			"final_length": len(d.content),
			"system_error": d.systemError,
		})

	if opts.IterationsOut != nil {
		*opts.IterationsOut = d.iterations
	}
}

// modelReply is how the model loop of a turn ended.
type modelReply struct {
	content      string
	normal       bool // the last response ended normally
	degenerate   bool // the reply stayed empty after every poke
	finishReason string
	iterations   int
	// images counts the images the model generated in the turn.
	images int
	// lastImages counts the images of the last response.
	lastImages int
	// imageReplies are the lines the reply carries about them (saved paths,
	// URLs, why one was not sent), from every response of the turn.
	imageReplies []string
}

// emptyReply decides what an empty model reply becomes: the text sent in its
// place and the turn's outcome. An empty text means the turn stays silent.
func (al *AgentLoop) emptyReply(agent *AgentInstance, opts processOptions, reply modelReply) (string, string) {
	fields := map[string]any{
		"agent_id":      agent.ID,
		"session_key":   opts.SessionKey,
		"finish_reason": reply.finishReason,
	}
	switch {
	case reply.normal && !reply.degenerate && opts.IsGroup:
		// An empty, normal reply in a group chat means the message was not for
		// this agent (e.g. @-directed at another).
		logger.DebugCF("agent", "empty normal response in group; staying silent", fields)
		return "", bus.OutcomeEmpty
	case reply.degenerate:
		// Reasoning but no reply, even after poking.
		logger.WarnCF("agent", "degenerate empty response; sending fallback reply", fields)
		return "Sorry — I couldn't compose a reply to that. Please try again.", bus.OutcomeEmpty
	case reply.normal:
		// A direct chat expects an answer, so an empty reply is a failure
		// (e.g. a degraded fallback model returning nothing): the user is
		// told rather than left waiting.
		logger.WarnCF("agent", "empty response on a direct message; advising the user", fields)
		return "The model returned an empty response. This can happen when a fallback model can't handle the request — please try again.", bus.OutcomeEmpty
	default:
		return fmt.Sprintf("The AI provider returned an empty response (finish reason: %s). Check provider logs for details.", reply.finishReason), bus.OutcomeError
	}
}

// reportSessionSave reports the save after a turn's reply was stored
// (TurnFinished): a failed save is logged and alerted; a good one ends the
// store-fault alerts, since the turn's messages were recorded and saved.
func (al *AgentLoop) reportSessionSave(agent *AgentInstance, sessionKey string, f hook.TurnResult) {
	if f.Stored == "" {
		return
	}
	if se, ok := errors.AsType[*hook.SessionStoreError](f.SaveErr); ok && f.Err == nil {
		logger.WarnCF("agent", "Failed to save session",
			map[string]any{"error": se.Err.Error(), "session": sessionKey})
		al.alertStoreFault(agent, alerter.Alert{
			Title:       alertSessionNotSaved,
			Description: "conversation history is being lost (disk full or unwritable?)",
			Details:     se.Err.Error(),
			EventID:     sessionStoreAlertID,
		})
		return
	}
	if f.SaveErr == nil && f.Err == nil {
		al.storeFaultOver(agent, alertSessionNotSaved, alertSessionWriteFailed)
	}
}

// Titles and the shared id of the session-store alerts. One id for every
// session and all three, so a full disk is one alert in the alerter's window.
const (
	alertSessionNotSaved       = "Session not saved"
	alertSessionWriteFailed    = "Session store write failed"
	alertSessionStateNotStored = "Session state not persisted"
	sessionStoreAlertID        = "session-store"
)

// alertStoreFault raises a, an alert about agent's session store. A store
// that refuses one write refuses the next, on every turn, so the alert is
// raised when that starts and then once a day while it lasts
// (al.storeFaults), with a note of since when and how many were held back;
// storeFaultOver ends it. Every failure is logged by the caller.
func (al *AgentLoop) alertStoreFault(agent *AgentInstance, a alerter.Alert) {
	out := al.Alerter()
	if _, none := out.(alerter.Nop); none {
		return // nobody to tell: the first failure with an alerter is the first alert
	}
	o := al.storeFaults.Report(storeFaultKey(agent, a.Title), al.clk().Now())
	if !o.Alert {
		return
	}
	a.Description += o.Note()
	out.Send(a)
}

// storeFaultOver records that agent's session store took the writes the
// alerts with these titles are about, so the next refusal alerts at once.
func (al *AgentLoop) storeFaultOver(agent *AgentInstance, titles ...string) {
	for _, title := range titles {
		al.storeFaults.Resolve(storeFaultKey(agent, title))
	}
}

// storeFaultKey names one alert about one session store. A configured agent
// has its own; a clone counts as its source and every other temporary agent
// as one, because they come and go by the dozen (one per sub-agent, one per
// forum participant) and a full disk must not alert once for each.
func storeFaultKey(agent *AgentInstance, title string) string {
	owner := agent.ID
	switch {
	case agent.Spec.IsClone():
		owner = agent.Spec.SourceID
	case agent.IsTemp():
		owner = "temporary agents"
	}
	return title + "\x00" + owner
}

// publishTurnReply sends the reply to the turn's chat, reporting its
// delivery to opts.OnReplyDelivery.
func (al *AgentLoop) publishTurnReply(ctx context.Context, opts processOptions, content string) {
	var onDelivery func(error)
	if opts.OnReplyDelivery != nil {
		onDelivery = func(err error) { opts.OnReplyDelivery(content, err) }
	}
	if err := al.bus.PublishOutbound(ctx, bus.OutboundMessage{
		Channel:    opts.Channel,
		ChatID:     opts.ChatID,
		Content:    content,
		OnDelivery: onDelivery,
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish response",
			map[string]any{"error": err.Error(), "channel": opts.Channel, "session": opts.SessionKey})
	}
}

// noResponseSentinel is the reply a model uses to decline responding (e.g. a
// group message addressed to someone else). It is treated as intentional
// silence: the typing indicator is cleared, nothing is sent, and the empty-
// response poke/fallback does NOT fire (distinguishing it from a real failure).
const noResponseSentinel = "!none"

// isNoResponseSentinel reports whether content is the model's "decline to reply"
// signal. Tolerant of surrounding quotes/backticks, whitespace, and any trailing
// text after the token — e.g. `"!none"`, "!none.", "!none — that's for Bob" all
// count — but not a longer word that merely starts with it (e.g. "!nonexistent").
func isNoResponseSentinel(content string) bool {
	s := strings.TrimSpace(content)
	// Strip any leading quotes/backticks the model may have wrapped it in; the
	// boundary check below tolerates a trailing quote/punctuation/text.
	s = strings.TrimSpace(strings.TrimLeft(s, "\"'`"))
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, noResponseSentinel) {
		return false
	}
	rest := s[len(noResponseSentinel):]
	if rest == "" {
		return true
	}
	// The next character must be a boundary (not a letter/digit), so a longer
	// token like "!nonexistent" does not match.
	c := rest[0]
	alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	return !alnum
}

// summarizeEvictions renders one consolidated notice for all of a turn's
// evictions: total count, total bytes freed, and a per-resource breakdown (top
// few by count). Keeps the chat to a single line even when an agent re-reads the
// same file every iteration; per-eviction detail lives in the DEBUG log.
func summarizeEvictions(events []ctxengine.EvictionEvent) string {
	totalBytes := 0
	counts := map[string]int{}
	var order []string
	for _, e := range events {
		totalBytes += e.Bytes
		if _, seen := counts[e.Resource]; !seen {
			order = append(order, e.Resource)
		}
		counts[e.Resource]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })

	const maxRes = 3
	parts := make([]string, 0, maxRes+1)
	for i, res := range order {
		if i >= maxRes {
			parts = append(parts, fmt.Sprintf("+%d more", len(order)-maxRes))
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", capEvictResource(res), counts[res]))
	}
	return fmt.Sprintf("🧹 Context evicted %d read(s) %s — %s",
		len(events), humanBytes(totalBytes), strings.Join(parts, ", "))
}

func humanBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.0f KB", float64(n)/1024.0)
	}
	return fmt.Sprintf("%d B", n)
}

// capEvictResource shortens an evicted resource for the notice: the file basename
// (paths are noise in a one-line notice), capped for a very long name.
func capEvictResource(s string) string {
	base := filepath.Base(s)
	const maxLen = 48
	if len(base) > maxLen {
		return base[:maxLen-1] + "…"
	}
	return base
}

// evictionNotifyUser reports whether the agent's resolved eviction policy has
// notify_user enabled, so the loop can surface a consolidated notice at the end
// of the turn. The DEBUG log of evictions is unconditional and happens inside
// the sweep.
func (al *AgentLoop) evictionNotifyUser(agent *AgentInstance) bool {
	cfg := al.GetConfig()
	p := ctxengine.DefaultEvictionPolicy()
	applyEvictionConfig(&p, cfg.Agents.Defaults.ContextEviction)
	if agent != nil && agent.Config != nil {
		applyEvictionConfig(&p, agent.Config.ContextEviction)
	}
	return p.NotifyUser
}

// assembleRequest builds the per-dispatch request for the context manager:
// the system-prompt layers for this conversation, the cost of the tool schemas
// this dispatch will send, the memory blocks recalled for the user's message
// this turn, and the channel the compaction reporter should answer on. mem
// may be nil.
func (al *AgentLoop) assembleRequest(ctx context.Context, agent *AgentInstance, mem *cogmem.Session, opts processOptions) ctxengine.AssembleRequest {
	defs := agent.Tools.ToolDefs(nil).Send
	if al.turnLimits(agent, opts).noTools {
		defs = nil
	}
	// The turn's request is not built yet: the stored history is what it
	// will carry. Read only when a deferred tool makes the history matter.
	var history []providers.Message
	if hasDeferredTool(defs) && agent.Sessions != nil {
		history = agent.Sessions.GetHistory(opts.SessionKey)
	}
	return al.assembleRequestWithDefs(ctx, agent, mem, opts, defs, history)
}

// assembleRequestWithDefs is assembleRequest for a caller that already holds
// this dispatch's tool definitions and the messages it will send (history).
func (al *AgentLoop) assembleRequestWithDefs(ctx context.Context, agent *AgentInstance, mem *cogmem.Session, opts processOptions, defs []providers.ToolDefinition, history []providers.Message) ctxengine.AssembleRequest {
	return ctxengine.AssembleRequest{
		ToolDefinitionTokens: ctxengine.EstimateToolDefinitionTokens(sentToolDefs(agent, defs, history)),
		Layers:               al.promptLayers(agent, opts),
		Injections:           recallInjections(ctx, mem, opts.UserMessage),
		Channel:              opts.Channel,
		ChatID:               opts.ChatID,
	}
}

// sentToolDefs is the part of the agent's tool list a request with history
// actually carries, and so what its tool schemas cost: the tools that are not
// deferred, plus the deferred ones a message in history references
// (discovery's InContext). An unreferenced deferred tool is not sent at all (a
// provider without native deferred tools leaves it out; a native one loads
// only referenced ones). defs is the dispatch's list: none means no tools are
// offered, so none cost anything.
func sentToolDefs(agent *AgentInstance, defs []providers.ToolDefinition, history []providers.Message) []providers.ToolDefinition {
	if len(defs) == 0 {
		return nil
	}
	return agent.Tools.ToolDefs(revealedIn(history)).InContext
}

// revealedIn is what history's tool results have revealed. The host keeps no
// revealed-tools state of its own: it is rebuilt from the messages a request
// carries, so it follows a restart and a compaction that drops messages.
func revealedIn(history []providers.Message) *discovery.Session {
	refs := make([][]string, 0, len(history))
	for _, m := range history {
		if len(m.ToolReferences) > 0 {
			refs = append(refs, m.ToolReferences)
		}
	}
	return discovery.NewSession(refs...)
}

// hasDeferredTool reports whether any definition is deferred.
func hasDeferredTool(defs []providers.ToolDefinition) bool {
	return slices.ContainsFunc(defs, common.IsDeferred)
}

// promptLayers is the system prompt for one dispatch: the agent's static and
// dynamic prompt for this channel/chat, then the session token behind the
// summary. A test that injects a bare instance without a ContextBuilder gets
// only the token layer. A fresh temporary agent has no tools, so no token:
// its prompt is exactly its creator's.
func (al *AgentLoop) promptLayers(agent *AgentInstance, opts processOptions) []ctxengine.Layer {
	var layers []ctxengine.Layer
	if agent.ContextBuilder != nil {
		layers = agent.ContextBuilder.PromptLayers(opts.Channel, opts.ChatID)
	}
	if agent.Spec.Fresh {
		return layers
	}
	return append(layers, sessionTokenLayer(al.sessionToken(agent, opts.SessionKey)))
}

// estimateTokens estimates the number of tokens in a message list.
// Uses a safe heuristic of 2.5 characters per token to account for CJK and other
// overheads better than the previous 3 chars/token.
func (al *AgentLoop) estimateTokens(messages []providers.Message) int {
	totalChars := 0
	for _, m := range messages {
		totalChars += utf8.RuneCountInString(m.Content)
	}
	// 2.5 chars per token = totalChars * 2 / 5
	return totalChars * 2 / 5
}

// reportStoreRefusal logs and alerts a turn that ended because the session
// store refused a write (a *hook.SessionStoreError in err); any other err is
// not the store's.
func (al *AgentLoop) reportStoreRefusal(agent *AgentInstance, sessionKey string, err error) {
	if se, ok := errors.AsType[*hook.SessionStoreError](err); ok {
		al.storeWriteFailed(agent, sessionKey, se.What, se.Err)
	}
}

// storeWriteFailed logs at error level and raises the operator alert for a
// what the session store refused to record.
func (al *AgentLoop) storeWriteFailed(agent *AgentInstance, sessionKey, what string, err error) {
	logger.ErrorCF("agent", "Session store write failed", map[string]any{
		"agent_id":    agent.ID,
		"session_key": sessionKey,
		"message":     what,
		"error":       err.Error(),
	})
	al.alertStoreFault(agent, alerter.Alert{
		Title:       alertSessionWriteFailed,
		Description: "a " + what + " could not be written to the session store and the turn was abandoned (disk full or unwritable?)",
		Details:     err.Error(),
		EventID:     sessionStoreAlertID,
	})
}

// storeRefusalError is the error a turn the session store refused what for
// ends with.
func storeRefusalError(what string, err error) error {
	return fmt.Errorf("session store rejected the %s, so the turn was abandoned: %w", what, err)
}
