// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
)

// storeThenSendContextManager fails the test when the turn's reply is
// stored after something was already sent to the chat.
type storeThenSendContextManager struct {
	capturingContextManager
	msgBus    *bus.MessageBus
	sentEarly atomic.Bool
	stored    atomic.Bool
}

func (m *storeThenSendContextManager) AddAssistantMessage(ctx context.Context, msg spawnllm.Message) (int64, error) {
	peek, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Millisecond)
	defer cancel()
	if _, ok := m.msgBus.SubscribeOutbound(peek); ok {
		m.sentEarly.Store(true)
	}
	m.stored.Store(true)
	return m.capturingContextManager.AddAssistantMessage(ctx, msg)
}

// A turn's reply is stored before it is sent.
func TestRunAgentLoop_ReplyStoredBeforeSent(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al := tl.al
	agent := al.registry.Default()
	setTestProvider(agent, &finalLLMProvider{})
	cm := &storeThenSendContextManager{msgBus: tl.msgBus}
	entry := &cmEntry{cm: cm, sessionKey: "stored-then-sent"}
	entry.touch()
	al.contextManagers.Store(agent.ID+":stored-then-sent", entry)

	reply, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey: "stored-then-sent", Channel: "telegram", ChatID: "alice", UserMessage: "hello", SendResponse: true,
	})
	if err != nil || reply == "" {
		t.Fatalf("runAgentLoop = %q, %v", reply, err)
	}
	if !cm.stored.Load() || cm.sentEarly.Load() {
		t.Fatalf("stored %v, something sent before it was stored %v", cm.stored.Load(), cm.sentEarly.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if msg, ok := tl.msgBus.SubscribeOutbound(ctx); !ok || msg.Content != reply {
		t.Fatalf("sent %q, %v; want the reply", msg.Content, ok)
	}
}

// The reply AfterTurn decided is sent only when the turn finished with the
// outcome it was decided for: a reply whose storing failed is not sent.
func TestSendModelReply_OnlyForTheDecidedOutcome(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	agent := tl.al.registry.Default()
	opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "alice", SendResponse: true}
	st := &turnState{reply: turnReply{decided: true, outcome: hook.OutcomeOK, content: "Hi Alice"}}
	sent := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, ok := tl.msgBus.SubscribeOutbound(ctx)
		return ok
	}

	failed := hook.TurnResult{
		Outcome: hook.OutcomeError, Reply: "Hi Alice",
		Err: &hook.SessionStoreError{What: "assistant reply", Err: errors.New("disk full")},
	}
	tl.al.sendModelReply(context.Background(), agent, opts, st, failed)
	if sent() {
		t.Fatal("a reply was sent for a turn whose reply was not stored")
	}
	tl.al.sendModelReply(context.Background(), agent, opts, st, hook.TurnResult{Outcome: hook.OutcomeOK, Reply: "Hi Alice"})
	if !sent() {
		t.Fatal("the reply was not sent")
	}
}

// The store-fault alert is raised from TurnFinished, for a turn the session
// store refused.
func TestTurnFinished_RaisesStoreFaultAlert(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	rec := &alertRecorder{}
	tl.al.SetAlerter(rec)
	agent := tl.al.registry.Default()
	h := tl.al.newTurnHooks(context.Background(), agent, processOptions{SessionKey: "s"}, "m")
	refused := &hook.SessionStoreError{What: "tool result", Err: errors.New("disk full")}

	h.emit(context.Background(), hook.ToolResultStored{})
	if len(rec.alerts) != 0 {
		t.Fatal("alerted before TurnFinished")
	}
	var fin hook.TurnFinished
	fin.Outcome, fin.Err = hook.OutcomeError, refused
	h.emit(context.Background(), fin)
	if len(rec.alerts) != 1 || rec.alerts[0].Title != alertSessionWriteFailed || rec.alerts[0].EventID != sessionStoreAlertID {
		t.Fatalf("alerts = %+v", rec.alerts)
	}
}

// The per-chat settings a turn's consumers depend on (/tools breadcrumbs,
// streaming, the fallback notice) are read when its model loop starts
// (BeforeLoop), not when its hooks are built: until then the consumers do
// nothing.
func TestLoopStart_ReadPerChatSettingsAtLoopStart(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al := tl.al
	agent := al.registry.Default()
	opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "alice"}
	al.setShowToolActivity(agent, opts.SessionKey, false)
	h, st := al.turnHooksFor(context.Background(), agent, &opts, "m", nil)
	if st.breadcrumbs || st.notifyFallback != nil || st.stream != nil || st.stopProgress != nil || st.pendingSource {
		t.Fatal("per-chat settings read before the loop starts")
	}
	// Until the loop starts the consumers do nothing, even with /tools on.
	al.setShowToolActivity(agent, opts.SessionKey, true)
	h.emit(context.Background(), hook.ToolCallStarted{Call: hook.ToolCall{ID: "c1", Name: "display_tool"}})
	peek, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if msg, ok := tl.msgBus.SubscribeOutbound(peek); ok {
		t.Fatalf("published before the loop started: %q", msg.Content)
	}
	loop := &hook.Loop{TextDeltas: len(h.onTextDelta) > 0}
	if !loop.TextDeltas {
		t.Fatal("the stream consumer is not registered when the turn is built")
	}
	if err := h.callBeforeLoop(context.Background(), loop); err != nil {
		t.Fatal(err)
	}
	if st.stopProgress != nil {
		st.stopProgress()
	}
	if !st.breadcrumbs || st.notifyFallback == nil || !st.pendingSource {
		t.Fatalf("at loop start: breadcrumbs %v, fallback notice %v, pending source %v",
			st.breadcrumbs, st.notifyFallback != nil, st.pendingSource)
	}
	if loop.TextDeltas || st.stream != nil {
		t.Fatal("a turn on a channel that does not stream asks for partial text")
	}
}

// A step-changing callback's panic ends a host turn as a panic in the turn
// always has: it reaches the caller, whose panic line carries the
// callback's own stack, and the sender's error text is today's.
func TestTurnOutcome_CallbackPanicReachesTheCaller(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	perr := panicOf(bobsPanickingCallback)
	var err error
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("turnOutcome did not panic")
			}
			err = messagePanicked(bus.InboundMessage{Channel: "telegram", ChatID: "alice"}, r)
		}()
		_, rerr := (&AgentLoop{}).turnOutcome(&hostTurn{}, hook.TurnResult{Err: perr})
		t.Fatalf("turnOutcome returned %v instead of panicking", rerr)
	}()
	if err == nil || err.Error() != "internal error while processing the message: Bob's bug" {
		t.Fatalf("error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "panic while processing message") || !strings.Contains(out, "bobsPanickingCallback") {
		t.Fatalf("log does not carry the callback's stack: %s", out)
	}
}

// bobsPanickingCallback is a callback with a bug.
func bobsPanickingCallback() { panic("Bob's bug") }

// panicOf is fn's panic as the agent reports a callback's: its value and
// the stack of fn.
func panicOf(fn func()) (perr *hook.PanicError) {
	defer func() {
		if r := recover(); r != nil {
			perr = &hook.PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// A panic in the turn engine itself (here the host's prompt code, which the
// engine calls) reaches the caller as it always has, after the turn ended:
// the panic line carries the stack of the code that panicked, not the
// caller's, and the sender's error text is today's.
func TestTurnOutcome_EnginePanicReachesTheCaller(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	var eperr *loop.EnginePanicError
	func() {
		defer func() {
			r := recover()
			eperr = &loop.EnginePanicError{Value: r, Stack: debug.Stack()}
		}()
		bobsPanickingCallback()
	}()
	var err error
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("turnOutcome did not panic")
			}
			err = messagePanicked(bus.InboundMessage{Channel: "telegram", ChatID: "alice"}, r)
		}()
		_, rerr := (&AgentLoop{}).turnOutcome(&hostTurn{}, hook.TurnResult{Err: eperr})
		t.Fatalf("turnOutcome returned %v instead of panicking", rerr)
	}()
	if err == nil || err.Error() != "internal error while processing the message: Bob's bug" {
		t.Fatalf("error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "panic while processing message") || !strings.Contains(out, "bobsPanickingCallback") {
		t.Fatalf("log does not carry the panicking code's stack: %s", out)
	}
	if strings.Contains(out, "TestTurnOutcome_EnginePanicReachesTheCaller.func2") {
		t.Fatalf("log carries the caller's stack, not the panicking code's: %s", out)
	}
}
