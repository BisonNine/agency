// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
)

// loopTurnEdgeCM is a context manager whose window and reset fail.
type loopTurnEdgeCM struct {
	ctxengine.ContextManager
	windowErr, resetErr error
}

func (m *loopTurnEdgeCM) SetContextWindow(n int) error {
	if m.windowErr != nil {
		return m.windowErr
	}
	return m.ContextManager.SetContextWindow(n)
}

func (m *loopTurnEdgeCM) Reset(ctx context.Context) error {
	if m.resetErr != nil {
		return m.resetErr
	}
	return m.ContextManager.Reset(ctx)
}

// A context window the manager refuses is logged and the turn goes on.
func TestLoopTurn_ContextWindowRefused(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()
	key := routing.BuildAgentMainSessionKey(agent.ID)
	_, release := al.getContextManager(agent, key)
	release()
	v, _ := al.contextManagers.Load(agent.ID + ":" + key)
	entry, ok := v.(*cmEntry)
	if !ok {
		t.Fatal("no cached session context")
	}
	entry.cm = &loopTurnEdgeCM{ContextManager: entry.cm, windowErr: errors.New("too small")}

	reply, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey: key, Channel: "subagent", ChatID: key, UserMessage: "hi",
	})
	if err != nil || reply != "Mock response" {
		t.Fatalf("runAgentLoop = %q, %v; want the model's answer", reply, err)
	}
	if !strings.Contains(logs.String(), "Context window not set for the turn's model") {
		t.Fatalf("the refused window was not logged: %s", logs.String())
	}
}

// Without a registry a turn begins on the agent it was given.
func TestLoopTurn_BeginTurnWithoutRegistry(t *testing.T) {
	agent := &AgentInstance{ID: "alice"}
	got, end, err := (&AgentLoop{}).beginAgentTurn(agent, "main")
	if err != nil || got != agent {
		t.Fatalf("beginAgentTurn = %v, %v; want the agent", got, err)
	}
	end()
}

// A session clear whose reset fails is logged and the turn goes on; a
// single-shot turn whose reset fails ends with the error.
func TestLoopTurn_PrepareSessionResetFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()
	cm := &loopTurnEdgeCM{resetErr: errors.New("locked")}

	opts := &processOptions{SessionKey: "s", ResetSession: true, UserMessage: "hi"}
	turn := &hook.Turn{}
	if err := al.prepareSession(context.Background(), agent, cm, opts, turn); err != nil {
		t.Fatalf("prepareSession = %v, want the turn to go on", err)
	}
	if !strings.Contains(logs.String(), "session_clear: Reset failed") || turn.Message.Text != "hi" {
		t.Fatalf("reset failure not logged or message lost (%q): %s", turn.Message.Text, logs.String())
	}

	single := &AgentInstance{ID: "s1", Spec: agentreg.Spec{ID: "s1", Fresh: true, Mode: agentreg.ModeSingleShot}}
	err := al.prepareSession(context.Background(), single, cm, &processOptions{SessionKey: "s"}, &hook.Turn{})
	if err == nil || err.Error() != "single-shot reset: locked" {
		t.Fatalf("prepareSession = %v, want the single-shot reset error", err)
	}
}

// A reply that is only the reply lines leaves no model content.
func TestLoopTurn_ModelReplyOnlyReplyLines(t *testing.T) {
	r := modelReplyOf(hook.TurnResult{Reply: "[image 1]"}, &turnState{replyLines: []string{"[image 1]"}})
	if r.content != "" || len(r.imageReplies) != 1 {
		t.Fatalf("modelReplyOf = %+v, want no content", r)
	}
}

// A reply of images alone reports its iterations and sends nothing.
func TestLoopTurn_SendImagesOnlyReply(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()
	n := 0
	st := &turnState{reply: turnReply{decided: true, outcome: hook.OutcomeOK, imagesOnly: true, iterations: 3, images: 2}}
	al.sendModelReply(context.Background(), agent, processOptions{Channel: "telegram", ChatID: "42", SendResponse: true, IterationsOut: &n}, st,
		hook.TurnResult{Outcome: hook.OutcomeOK})
	if n != 3 {
		t.Fatalf("iterations = %d, want 3", n)
	}
	noOutbound(t, msgBus)
}

// A session the store failed to save is logged and alerted.
func TestLoopTurn_SessionSaveFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := hooksEdgeLoop(t)
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	agent := al.GetRegistry().Default()
	al.reportSessionSave(agent, "s", hook.TurnResult{
		Stored: "reply", SaveErr: &hook.SessionStoreError{What: "session save", Err: errors.New("disk full")},
	})
	if !strings.Contains(logs.String(), "Failed to save session") {
		t.Fatalf("the failed save was not logged: %s", logs.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.alerts) != 1 || rec.alerts[0].Title != alertSessionNotSaved || rec.alerts[0].Details != "disk full" {
		t.Fatalf("alerts = %+v, want one session-not-saved alert", rec.alerts)
	}
}

// A reply the bus refuses is logged.
func TestLoopTurn_PublishReplyFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksEdgeLoop(t)
	msgBus.Close()
	al.publishTurnReply(context.Background(), processOptions{Channel: "telegram", ChatID: "42"}, "hello")
	if !strings.Contains(logs.String(), "Failed to publish response") {
		t.Fatalf("the refused reply was not logged: %s", logs.String())
	}
}

// A very long evicted resource name is cut with an ellipsis.
func TestLoopTurn_CapEvictResource(t *testing.T) {
	got := capEvictResource("/a/b/" + strings.Repeat("x", 60) + ".txt")
	if len([]rune(got)) != 48 || !strings.HasSuffix(got, "…") {
		t.Fatalf("capEvictResource = %q (%d runes), want 48 ending in an ellipsis", got, len([]rune(got)))
	}
	if got := capEvictResource("/a/b/c.txt"); got != "c.txt" {
		t.Fatalf("capEvictResource = %q, want the basename", got)
	}
}

// Tokens are estimated at 2.5 characters each, counted in runes.
func TestLoopTurn_EstimateTokens(t *testing.T) {
	got := (&AgentLoop{}).estimateTokens([]spawnllm.Message{
		{Content: strings.Repeat("a", 10)},
		{Content: strings.Repeat("é", 15)},
	})
	if got != 10 {
		t.Fatalf("estimateTokens = %d, want 10", got)
	}
}
