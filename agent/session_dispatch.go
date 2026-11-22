// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/constants"
)

// sessionIdleTTL is how long a session's dispatch state survives with no turn
// running or queued before pruneSessions drops it.
const sessionIdleTTL = time.Hour

// errCancelledByUser is the cause /cancel gives the running turn's context, so
// the turn's failure renders as a cancellation rather than a timeout.
var errCancelledByUser error = &turnCauseError{text: "cancelled by /cancel", kind: hook.ErrInterrupted}

// userCancelError is the cause one /cancel gives what it stops: errCancelledByUser,
// with the chat it was typed in (queuedChat), which gets the one reply.
type userCancelError struct{ chat string }

func (c *userCancelError) Error() string { return errCancelledByUser.Error() }
func (c *userCancelError) Unwrap() error { return errCancelledByUser }

// cancelChat is the chat of the /cancel behind cause; empty for any other
// cause.
func cancelChat(cause error) string {
	if c, ok := errors.AsType[*userCancelError](cause); ok {
		return c.chat
	}
	return ""
}

// sessionState is one session's dispatch state. The first message to arrive
// while the session is idle makes its goroutine the owner: it runs turns until
// pending is empty, so at most one goroutine per session is ever running or
// waiting. Messages arriving while the owner is busy are appended to pending
// and their goroutines return at once; the owner then runs each chat's queued
// messages as one merged turn (see takeBatch). refs and lastUsed are guarded by
// AgentLoop.sessionsMu; everything else by mu.
type sessionState struct {
	refs     int       // goroutines holding the entry; pruned only at zero
	lastUsed time.Time // set on release

	mu         sync.Mutex
	busy       bool                    // an owner goroutine is draining pending
	pending    []bus.InboundMessage    // arrived while busy, in arrival order
	turnCancel context.CancelCauseFunc // cancels the running turn; nil when idle
	// handles holds the running turn's engine handle once its engine turn
	// has started; nil when idle.
	handles *turnHandles
	// cancelledRunning and skipCount record what /cancel did, for its reply.
	cancelledRunning bool
	skipCount        int
}

// acquireSession returns the dispatch state for key, creating it if needed, and
// takes a reference that keeps pruneIdleSessions from dropping it.
func (al *AgentLoop) acquireSession(key string) *sessionState {
	al.sessionsMu.Lock()
	defer al.sessionsMu.Unlock()
	if al.sessions == nil {
		al.sessions = make(map[string]*sessionState)
	}
	ss := al.sessions[key]
	if ss == nil {
		ss = &sessionState{}
		al.sessions[key] = ss
	}
	ss.refs++
	return ss
}

// releaseSession drops the reference taken by acquireSession.
func (al *AgentLoop) releaseSession(ss *sessionState) {
	al.sessionsMu.Lock()
	ss.refs--
	ss.lastUsed = time.Now()
	al.sessionsMu.Unlock()
}

// pruneIdleSessions drops every session entry with no holder that has been idle
// longer than ttl, returning how many it removed. A held entry (a turn running
// or queued, or a /cancel in flight) is never removed.
func (al *AgentLoop) pruneIdleSessions(now time.Time, ttl time.Duration) int {
	al.sessionsMu.Lock()
	defer al.sessionsMu.Unlock()
	removed := 0
	for key, ss := range al.sessions {
		if ss.refs == 0 && now.Sub(ss.lastUsed) > ttl {
			delete(al.sessions, key)
			removed++
		}
	}
	return removed
}

// pruneSessions runs until evictStop is closed, dropping idle session state
// every evictInterval.
func (al *AgentLoop) pruneSessions() {
	ticker := al.clk().NewTicker(al.evictInterval)
	defer ticker.Stop()
	for {
		select {
		case <-al.evictStop:
			return
		case <-ticker.C():
			al.pruneIdleSessions(al.clk().Now(), sessionIdleTTL)
		}
	}
}

// cancel stops the session's running turn, if any, and drops everything queued
// behind it, recording both for the /cancel reply. It returns the dropped
// messages.
func (ss *sessionState) cancel(ctx context.Context, cause error) []bus.InboundMessage {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	dropped := ss.pending
	ss.skipCount += len(ss.pending)
	ss.pending = nil
	if ss.turnCancel != nil {
		// The engine turn is stopped through its handle (Interrupt, with
		// the host's cause), the turn's context with it: a turn still
		// waiting for its slot, or past its engine turn, ends too.
		if h := ss.handles.current(); h != nil {
			h.cancelFrom(ctx, cause)
		}
		ss.turnCancel(cause)
		ss.turnCancel = nil
		ss.handles = nil
		ss.cancelledRunning = true
	}
	return dropped
}

// turnHandles records the engine handle of a queued turn: the first engine
// turn started on its context (the turn's own; a nested one never claims
// it).
type turnHandles struct {
	mu      sync.Mutex
	h       *turnHandle
	claimed bool
}

type turnHandlesKey struct{}

// withTurnHandles returns ctx on which the first engine turn records its
// handle in hs.
func withTurnHandles(ctx context.Context, hs *turnHandles) context.Context {
	return context.WithValue(ctx, turnHandlesKey{}, hs)
}

// claimTurnHandle is the onHandle of the first engine turn on ctx; nil for
// any other.
func claimTurnHandle(ctx context.Context) func(*turnHandle) {
	hs, ok := ctx.Value(turnHandlesKey{}).(*turnHandles)
	if !ok || hs == nil {
		return nil
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.claimed {
		return nil
	}
	hs.claimed = true
	return func(h *turnHandle) {
		hs.mu.Lock()
		hs.h = h
		hs.mu.Unlock()
	}
}

// current is the recorded handle; nil before the engine turn started.
func (hs *turnHandles) current() *turnHandle {
	if hs == nil {
		return nil
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.h
}

// takeCancelResult reads and resets what /cancel recorded for the session:
// whether it stopped a running turn and how many queued messages it dropped.
// Nothing is recorded for an unknown session.
func (al *AgentLoop) takeCancelResult(key string) (running bool, skipped int) {
	al.sessionsMu.Lock()
	ss := al.sessions[key]
	al.sessionsMu.Unlock()
	if ss == nil {
		return false, 0
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	running, skipped = ss.cancelledRunning, ss.skipCount
	ss.cancelledRunning, ss.skipCount = false, 0
	return running, skipped
}

// queuedChat is the chat a queued message belongs to, for takeBatch: its
// channel and chat ID, and for a system message the channel its chat ID
// belongs to (bus.MetaOriginChannel), so results for two chats with the same
// ID on different channels are never merged.
func queuedChat(m bus.InboundMessage) string {
	return m.Channel + ":" + inboundMetadata(m, bus.MetaOriginChannel) + ":" + m.ChatID
}

// takeBatch removes the next turn's messages from pending and returns them
// merged: the oldest message plus every later message from the same
// channel:chat, up to the next command in that chat. Messages from other chats
// stay queued (an agent's session may serve several chats, whose messages are
// never merged), and a command always runs as a turn of its own, as does a
// message that requires its own reply (bus.MetaReplyRequired). With
// eachAlone (the session's agent is single-shot) every message is a turn of
// its own.
func takeBatch(pending *[]bus.InboundMessage, eachAlone bool) (bus.InboundMessage, bool) {
	queue := *pending
	if len(queue) == 0 {
		return bus.InboundMessage{}, false
	}
	first := queue[0]
	chat := queuedChat(first)
	batch := []bus.InboundMessage{first}
	rest := make([]bus.InboundMessage, 0, len(queue)-1)
	stop := eachAlone || runsAlone(first)
	for _, m := range queue[1:] {
		if !stop && queuedChat(m) == chat {
			if !runsAlone(m) {
				batch = append(batch, m)
				continue
			}
			stop = true
		}
		rest = append(rest, m)
	}
	*pending = rest
	return mergeMessages(batch), true
}

// runsAlone reports whether a message must be a turn of its own, never merged
// with others: a command, or a message that requires its own final reply.
func runsAlone(m bus.InboundMessage) bool {
	return commands.HasCommandPrefix(m.Content) || m.ReplyRequired()
}

// mergeMessages joins a batch into one message: the contents newline-separated
// in arrival order, every attachment kept, and the reply addressed to the last
// message.
func mergeMessages(batch []bus.InboundMessage) bus.InboundMessage {
	if len(batch) == 1 {
		return batch[0]
	}
	merged := batch[len(batch)-1]
	parts := make([]string, 0, len(batch))
	var media []string
	for _, m := range batch {
		parts = append(parts, m.Content)
		media = append(media, m.Media...)
	}
	merged.Content = strings.Join(parts, "\n")
	merged.Media = media
	// Someone's message merged with claw's own is someone's.
	merged.Internal = !slices.ContainsFunc(batch, func(m bus.InboundMessage) bool { return !m.Internal })
	return merged
}

// acquireTurnSlot waits for a free concurrent-turn slot, or returns false when
// ctx ends first. Always true when no limit is configured.
func (al *AgentLoop) acquireTurnSlot(ctx context.Context) bool {
	if al.turnSem == nil {
		return true
	}
	// The first select only notices the wait (AgentLoop.onWait).
	select {
	case al.turnSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	default:
	}
	al.notifyWait(waitTurnSlot)
	select {
	case al.turnSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (al *AgentLoop) releaseTurnSlot() {
	if al.turnSem != nil {
		<-al.turnSem
	}
}

// isCancelCommand returns true if the message content is a /cancel command.
func (al *AgentLoop) isCancelCommand(content string) bool {
	name, ok := commands.ParseCommandName(content)
	return ok && name == "cancel"
}

// processSessionMessage dispatches one inbound message within its session.
// Messages sharing a session scope key (resolved via resolveMessageRoute) never
// process concurrently — including channel:chatID pairs that map to the same
// agent session — while different sessions never block each other. When the
// session is busy the message is queued for the owner goroutine and this one
// returns; a /cancel skips the queue entirely, stopping the running turn and
// dropping what is queued behind it before replying.
func (al *AgentLoop) processSessionMessage(ctx context.Context, msg bus.InboundMessage) {
	defer al.activeRequests.Done()

	// A person writing in their human agent's chat answers the request
	// waiting for them, or is told nothing is; neither is a turn. Decided
	// before the session is taken: the turn waiting for the answer holds it.
	if al.handleHumanChat(ctx, msg) {
		return
	}

	// The mention is stripped and recorded before routing, so mention routing
	// is reflected in the dispatch key and the session key.
	al.extractMention(&msg)
	dispatchKey, singleShot := al.dispatchKey(&msg)

	ss := al.acquireSession(dispatchKey)
	defer al.releaseSession(ss)

	if al.isCancelCommand(msg.Content) {
		cause := &userCancelError{chat: queuedChat(msg)}
		dropped := ss.cancel(ctx, cause)
		al.publishCancelledReplies(ctx, dropped, cause)
		al.runTurn(ctx, ctx, msg)
		return
	}

	ss.mu.Lock()
	ss.pending = append(ss.pending, msg)
	if ss.busy {
		ss.mu.Unlock()
		return
	}
	ss.busy = true
	ss.mu.Unlock()
	al.drainSession(ctx, ss, singleShot)
}

// dispatchKey resolves the route of msg so that every channel:chatID pair
// sharing an agent session gets the same key, preventing concurrent history
// reads and writes on the agent's one session. The session key is stamped
// on msg, so processMessage's own resolveScopeKey returns the same value.
// When routing fails the key falls back to channel:chatID; processMessage
// returns the same error and reports it. singleShot reports a single-shot
// agent, whose queued messages are never merged into one turn: it answers
// every message on a blank context.
func (al *AgentLoop) dispatchKey(msg *bus.InboundMessage) (string, bool) {
	route, routed, err := al.resolveMessageRoute(*msg)
	if err != nil {
		return msg.Channel + ":" + msg.ChatID, false
	}
	scopeKey := resolveScopeKey(route, msg.SessionKey)
	msg.SessionKey = scopeKey
	return scopeKey, routed != nil && routed.Spec.SingleShot()
}

// drainSession runs the session's queued messages as turns, one at a time,
// until none is left. The calling goroutine owns the session meanwhile.
func (al *AgentLoop) drainSession(ctx context.Context, ss *sessionState, singleShot bool) {
	for {
		ss.mu.Lock()
		batch, ok := takeBatch(&ss.pending, singleShot)
		if !ok || ctx.Err() != nil {
			ss.busy = false
			ss.mu.Unlock()
			return
		}
		// Registered in the same critical section as the take, so a /cancel
		// always sees the message either queued or running.
		turnCtx, cancelTurn := context.WithCancelCause(ctx)
		handles := &turnHandles{}
		turnCtx = withTurnHandles(turnCtx, handles)
		ss.turnCancel = cancelTurn
		ss.handles = handles
		ss.mu.Unlock()

		al.runQueuedTurn(ctx, turnCtx, batch)

		ss.mu.Lock()
		ss.turnCancel = nil
		ss.handles = nil
		ss.mu.Unlock()
		cancelTurn(nil)
	}
}

// chatHolder marks a turn running in a chat (channels.Manager.HoldChat).
type chatHolder interface {
	HoldChat(channel, chatID string) (release func())
}

// runQueuedTurn runs one batch as a turn holding a concurrent-turn slot.
// An asked turn takes no slot: its asker lent its own while it waits, so with
// every slot held by askers it could never run. Asks are bounded by
// max_subagent_depth instead. Any other turn's slot travels with it, so a
// wait inside it (an Ask, a request to a person) can lend it out (turnSlot).
// While it waits and runs, the chat's typing indicator, reaction and
// placeholder are held (channels.Manager.HoldChat), so a long turn keeps them.
func (al *AgentLoop) runQueuedTurn(ctx, turnCtx context.Context, batch bus.InboundMessage) {
	if al.chatHolder != nil && batch.ChatID != "" {
		defer al.chatHolder.HoldChat(batch.Channel, batch.ChatID)()
	}
	if batch.Channel == constants.AgentMessageChannel {
		al.runTurn(ctx, turnCtx, batch)
		return
	}
	slot := &turnSlot{al: al}
	if slot.acquire(turnCtx) {
		defer slot.release()
		al.runTurn(ctx, withTurnSlot(turnCtx, slot), batch)
		return
	}
	if cause := context.Cause(turnCtx); errors.Is(cause, errCancelledByUser) {
		// Cancelled while waiting for a turn slot: it never ran.
		al.publishCancelledReplies(ctx, []bus.InboundMessage{batch}, cause)
	}
}
