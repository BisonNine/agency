// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// storeThenSendContextManager fails the test when the turn's reply is
// stored after something was already sent to the chat.
type storeThenSendContextManager struct {
	capturingContextManager
	msgBus    *bus.MessageBus
	sentEarly atomic.Bool
	stored    atomic.Bool
}

func (m *storeThenSendContextManager) AddAssistantMessage(ctx context.Context, msg providers.Message) (int64, error) {
	peek, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
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

	failed := hook.TurnResult{Outcome: hook.OutcomeError, Reply: "Hi Alice",
		Err: &hook.SessionStoreError{What: "assistant reply", Err: errors.New("disk full")}}
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
	h.emit(context.Background(), hook.TurnFinished{TurnResult: hook.TurnResult{Outcome: hook.OutcomeError, Err: refused}})
	if len(rec.alerts) != 1 || rec.alerts[0].Title != alertSessionWriteFailed || rec.alerts[0].EventID != sessionStoreAlertID {
		t.Fatalf("alerts = %+v", rec.alerts)
	}
}

// The per-chat settings a turn's plugins depend on (/tools breadcrumbs,
// streaming, the fallback notice) are read when its model loop starts, not
// when its hooks are built.
func TestLoopPlugins_ReadPerChatSettingsAtLoopStart(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al := tl.al
	agent := al.registry.Default()
	opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "alice"}
	al.setShowToolActivity(agent, opts.SessionKey, false)
	h, st := al.turnHooksFor(context.Background(), agent, &opts, "m", nil)
	for _, c := range h.onEvent {
		if c.plugin == "breadcrumbs" {
			t.Fatal("breadcrumbs registered before the loop starts")
		}
	}
	if len(h.onFallback) != 0 || len(h.onTextDelta) != 0 {
		t.Fatal("per-chat plugins registered before the loop starts")
	}
	al.setShowToolActivity(agent, opts.SessionKey, true)
	var names []string
	for _, p := range al.loopPlugins(context.Background(), agent, opts, st)(context.Background()) {
		names = append(names, p.Name)
	}
	if st.stopProgress != nil {
		st.stopProgress()
	}
	if !containsString(names, "breadcrumbs") || !containsString(names, "fallback-notice") {
		t.Fatalf("plugins at loop start = %v", names)
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
