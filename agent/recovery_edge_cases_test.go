// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/ctxengine/session"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/state"
)

// recEdgeListFails is a session store that cannot list its pending sessions.
type recEdgeListFails struct{ session.SessionStore }

func (recEdgeListFails) ListPendingSessions() ([]string, error) {
	return nil, errors.New("disk unreadable")
}

// recEdgeClearFails is the recovery test store whose pending flag cannot be
// cleared.
type recEdgeClearFails struct{ *recoveryTestStore }

func (s recEdgeClearFails) ClearPendingTurn(key string) error {
	if err := s.recoveryTestStore.ClearPendingTurn(key); err != nil {
		return err
	}
	return errors.New("disk full")
}

// recEdgeReadOnlyState makes agent main's restart-recovery state unwritable
// until the test ends.
func recEdgeReadOnlyState(t *testing.T, al *AgentLoop) {
	t.Helper()
	dir := filepath.Join(mustGetAgent(t, al).Workspace, "state")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})
}

// An agent whose pending sessions cannot be listed is skipped with a
// warning; recovery carries on with the other agents and replays nothing for
// it.
func TestRecovery_RecoverPendingTurnsListFails(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	agent := mustGetAgent(t, tl.al)
	store := agent.Sessions
	agent.Sessions = recEdgeListFails{store}
	t.Cleanup(func() { agent.Sessions = store })

	tl.al.recoverPendingTurns(context.Background())

	if !strings.Contains(buf.String(), "Failed to list pending sessions for recovery") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
	noInbound(t, tl.msgBus)
}

// A pending flag that cannot be cleared is logged; the recorded source is
// cleared all the same.
func TestRecovery_ClearPendingFailureLogged(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl, store := newRecoveryTestLoop(t)

	tl.al.recoverSession(context.Background(), "main", testSessionKey, recEdgeClearFails{store})

	if !store.clearCalled {
		t.Fatal("ClearPendingTurn not called")
	}
	if !strings.Contains(buf.String(), "Failed to clear pending-turn flag") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
	noInbound(t, tl.msgBus)
}

// A session of an agent the configuration no longer has is cleared, not
// replayed.
func TestRecovery_RecoverSessionUnknownAgent(t *testing.T) {
	tl, store := newRecoveryTestLoop(t)
	const key = "agent:ghost:main"

	tl.al.recoverSession(context.Background(), "ghost", key, store)

	if !store.clearCalled || store.pendingCleared != key {
		t.Fatalf("pending flag not cleared for %s (called=%v key=%q)", key, store.clearCalled, store.pendingCleared)
	}
	noInbound(t, tl.msgBus)
	noOutbound(t, tl.msgBus)
}

// When the attempt count cannot be saved the turn is not replayed (it could
// crash-loop): the flag is cleared and the chat is told to resend.
func TestRecovery_RecoverSessionCannotPersistAttempts(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl, store := newRecoveryTestLoop(t)
	recordSource(t, tl.al, "webui", "chat-7")
	recEdgeReadOnlyState(t, tl.al)

	tl.al.recoverSession(context.Background(), "main", testSessionKey, store)

	if !store.clearCalled {
		t.Fatal("pending flag not cleared")
	}
	out, ok := consumeOutbound(t, tl.msgBus)
	if !ok || out.Content != recoveryGiveUpNotice || out.ChatID != "chat-7" {
		t.Fatalf("outbound = %+v (ok=%v), want the give-up notice to chat-7", out, ok)
	}
	noInbound(t, tl.msgBus)
}

// With the bus refusing (the context ended), the notice and the replay are
// both logged as failed and nothing is queued.
func TestRecovery_RecoverSessionPublishFails(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl, store := newRecoveryTestLoop(t)
	sm := recordSource(t, tl.al, "webui", "chat-7")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tl.al.recoverSession(ctx, "main", testSessionKey, store)

	logs := buf.String()
	for _, want := range []string{"Failed to publish recovery notice", "Failed to queue recovery message"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if pt, ok := sm.GetPendingTurn(testSessionKey); !ok || pt.Attempts != 1 {
		t.Fatalf("recorded source = %+v (ok=%v), want the attempt counted", pt, ok)
	}
	noInbound(t, tl.msgBus)
	noOutbound(t, tl.msgBus)
}

// A required give-up reply the bus refuses is logged.
func TestRecovery_RequiredGiveUpPublishFails(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tl.al.publishRequiredGiveUp(ctx, state.PendingTurn{Channel: "webui", ChatID: "c1", ReplyRequired: true})

	if !strings.Contains(buf.String(), "Failed to publish recovery give-up reply") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
	noOutbound(t, tl.msgBus)
}

// Recording and clearing a turn's source on unwritable state is logged, not
// fatal; what was on disk stays as it was.
func TestRecovery_PendingTurnSourceWriteFails(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	agent := mustGetAgent(t, tl.al)
	recordSource(t, tl.al, "webui", "chat-7")
	recEdgeReadOnlyState(t, tl.al)

	tl.al.recordPendingTurnSource(context.Background(), agent,
		processOptions{SessionKey: "agent:main:other", Channel: "webui", ChatID: "x"})
	tl.al.clearPendingTurnSource("main", testSessionKey)

	logs := buf.String()
	for _, want := range []string{"Failed to record pending turn source", "Failed to clear pending turn source"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	onDisk := state.NewManager(agent.Workspace, 0o700, 0o600)
	if pt, ok := onDisk.GetPendingTurn(testSessionKey); !ok || pt.ChatID != "chat-7" {
		t.Fatalf("source on disk = %+v (ok=%v), want chat-7 kept", pt, ok)
	}
	if _, ok := onDisk.GetPendingTurn("agent:main:other"); ok {
		t.Fatal("a failed record reached the disk")
	}
}
