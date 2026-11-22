// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// blockingProvider records the user message of every Chat call and holds each
// call until the test releases it (one send on release per call) or its context
// ends.
type blockingProvider struct {
	mu      sync.Mutex
	calls   []string
	started chan struct{}
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}, 32), release: make(chan struct{})}
}

func (p *blockingProvider) Chat(ctx context.Context, _ providers.Identity, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	user := ""
	for _, m := range slices.Backward(messages) {
		if m.Role == "user" {
			user = m.Content
			break
		}
	}
	p.mu.Lock()
	p.calls = append(p.calls, user)
	p.mu.Unlock()
	p.started <- struct{}{}
	select {
	case <-p.release:
		return &providers.LLMResponse{Content: "ok"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *blockingProvider) GetDefaultModel() string { return "mock-model" }

func (p *blockingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *blockingProvider) call(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[i]
}

// waitStarted fails the test unless the provider begins another call in time.
func (p *blockingProvider) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider call did not start")
	}
}

// dispatch hands msg to the loop the way Run does; it returns when
// processSessionMessage does (at once for a message merged into a busy session).
func dispatch(al *AgentLoop, msg bus.InboundMessage) {
	al.activeRequests.Add(1)
	al.processSessionMessage(context.Background(), msg)
}

func inbound(chat, id, content string) bus.InboundMessage {
	return bus.InboundMessage{Channel: "test", ChatID: chat, SenderID: "u1", MessageID: id, Content: content}
}

// nextOutbound returns the next reply the loop published, or fails the test.
// The bound is generous: it only matters when no reply comes, and an agent's
// first turn creates its session stores, which can take seconds on a busy
// disk.
func nextOutbound(t *testing.T, msgBus *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, ok := msgBus.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("no outbound reply")
	}
	return out
}

func newBlockingLoop(t *testing.T, cfg *config.Config) (*AgentLoop, *bus.MessageBus, *blockingProvider) {
	t.Helper()
	msgBus := bus.NewMessageBus()
	provider := newBlockingProvider()
	return mustNewAgentLoop(t, cfg, msgBus, provider, nil), msgBus, provider
}

// TestInbound_BurstMergesIntoOneFollowUpTurn: five messages from one chat while
// the first is running become one running turn plus one merged turn whose
// content is the queued messages joined in arrival order, replying to the last.
func TestInbound_BurstMergesIntoOneFollowUpTurn(t *testing.T) {
	al, msgBus, p := newBlockingLoop(t, newTestConfig(t))

	go dispatch(al, inbound("c1", "id1", "m1"))
	p.waitStarted(t)
	for _, m := range []string{"m2", "m3", "m4", "m5"} {
		dispatch(al, inbound("c1", "id"+m[1:], m)) // returns at once: session busy
	}

	p.release <- struct{}{}
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "id1" {
		t.Fatalf("first reply to %q, want id1", out.OriginalMessageID)
	}
	p.waitStarted(t)
	p.release <- struct{}{}
	out := nextOutbound(t, msgBus)
	if out.OriginalMessageID != "id5" {
		t.Fatalf("merged reply to %q, want id5", out.OriginalMessageID)
	}
	al.activeRequests.Wait()

	if n := p.callCount(); n != 2 {
		t.Fatalf("provider calls = %d, want 2 (one running + one merged)", n)
	}
	if got := p.call(1); !strings.Contains(got, "m2\nm3\nm4\nm5") {
		t.Fatalf("merged turn content = %q, want the four queued messages joined in order", got)
	}
}

// TestInbound_DifferentChatsNotMerged: two chats sharing one unified session
// are serialized but never merged into one turn.
func TestInbound_DifferentChatsNotMerged(t *testing.T) {
	al, msgBus, p := newBlockingLoop(t, newTestConfig(t))

	go dispatch(al, inbound("c1", "id1", "m1"))
	p.waitStarted(t)
	dispatch(al, inbound("c1", "id2", "m2"))
	dispatch(al, inbound("c2", "id3", "m3"))
	dispatch(al, inbound("c1", "id4", "m4"))

	for range 3 {
		p.release <- struct{}{}
		nextOutbound(t, msgBus)
		if p.callCount() < 3 {
			p.waitStarted(t)
		}
	}
	al.activeRequests.Wait()

	if n := p.callCount(); n != 3 {
		t.Fatalf("provider calls = %d, want 3 (m1, m2+m4, m3)", n)
	}
	if got := p.call(1); !strings.Contains(got, "m2\nm4") || strings.Contains(got, "m3") {
		t.Fatalf("second turn = %q, want c1's m2 and m4 only", got)
	}
	if got := p.call(2); !strings.HasSuffix(got, "m3") {
		t.Fatalf("third turn = %q, want c2's m3 alone", got)
	}
}

// twoAgentConfig routes channel "chb" to agent b and everything else to the
// default agent a, giving two independent sessions.
func twoAgentConfig(t *testing.T, maxConcurrent int) *config.Config {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.Agents.Defaults.MaxConcurrentTurns = maxConcurrent
	cfg.Agents.List = []config.AgentConfig{
		{ID: "a", Name: "A", Default: true},
		{ID: "b", Name: "B"},
	}
	cfg.Bindings = []config.AgentBinding{{AgentID: "b", Match: config.BindingMatch{Channel: "chb"}}}
	return cfg
}

// TestInbound_MaxConcurrentTurnsLimitsConcurrency: with one slot, the second
// session's turn does not start until the first finishes; unlimited, both run.
func TestInbound_MaxConcurrentTurnsLimitsConcurrency(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limit    int
		expectAt int // provider calls started while the first turn is held
	}{
		{name: "limit 1 serializes", limit: 1, expectAt: 1},
		{name: "unlimited runs both", limit: 0, expectAt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, msgBus, p := newBlockingLoop(t, twoAgentConfig(t, tc.limit))
			var slotWait <-chan struct{}
			al.onWait, slotWait = waitSignals(waitTurnSlot)

			go dispatch(al, bus.InboundMessage{Channel: "cha", ChatID: "x", SenderID: "u", Content: "first"})
			p.waitStarted(t)
			go dispatch(al, bus.InboundMessage{Channel: "chb", ChatID: "y", SenderID: "u", Content: "second"})
			if tc.expectAt == 2 {
				p.waitStarted(t)
			} else {
				awaitSignal(t, slotWait, "the second turn to wait for the slot")
			}
			if n := p.callCount(); n != tc.expectAt {
				t.Fatalf("calls started while first turn held = %d, want %d", n, tc.expectAt)
			}

			p.release <- struct{}{}
			nextOutbound(t, msgBus)
			if tc.expectAt == 1 {
				p.waitStarted(t)
			}
			p.release <- struct{}{}
			nextOutbound(t, msgBus)
			al.activeRequests.Wait()
		})
	}
}

// TestInbound_CancelStopsRunningTurn: /cancel bypasses the session queue,
// cancels the running turn (the provider sees its context end), drops the
// queued message, and says both in one reply: the stopped message, from the
// same chat, gets no reply of its own.
func TestInbound_CancelStopsRunningTurn(t *testing.T) {
	al, msgBus, p := newBlockingLoop(t, newTestConfig(t))

	go dispatch(al, inbound("c1", "id1", "m1"))
	p.waitStarted(t)
	dispatch(al, inbound("c1", "id2", "m2"))
	dispatch(al, inbound("c1", "id3", "/cancel")) // runs without waiting for the turn

	out := nextOutbound(t, msgBus)
	al.activeRequests.Wait()
	noOutbound(t, msgBus)

	if out.OriginalMessageID != "id3" || out.Content != "Main: stopped, and skipped 1 queued message." {
		t.Fatalf("reply = %+v, want the one /cancel report", out)
	}
	if n := p.callCount(); n != 1 {
		t.Fatalf("provider calls = %d, want 1 (m2 dropped)", n)
	}
}

// TestInbound_CancelSameChatRequiredReplies: a stopped message and a
// dropped one that require a reply are closed with the cancelled outcome
// and no text of their own when the /cancel was typed in their chat.
func TestInbound_CancelSameChatRequiredReplies(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()
	al, msgBus, p := newBlockingLoop(t, newTestConfig(t))

	go dispatch(al, required(inbound("c1", "id1", "m1")))
	p.waitStarted(t)
	dispatch(al, required(inbound("c1", "id2", "m2")))
	dispatch(al, inbound("c1", "id3", "m3"))
	dispatch(al, inbound("c1", "id4", "/cancel"))

	got := map[string]bus.OutboundMessage{}
	for range 3 {
		out := nextOutbound(t, msgBus)
		got[out.OriginalMessageID] = out
	}
	al.activeRequests.Wait()
	noOutbound(t, msgBus)

	for _, id := range []string{"id1", "id2"} {
		if out := got[id]; out.Outcome != bus.OutcomeCancelled || out.Content != "" {
			t.Fatalf("%s = %+v, want a cancelled close without text", id, out)
		}
	}
	if out := got["id4"]; out.Content != "Main: stopped, and skipped 2 queued messages." {
		t.Fatalf("/cancel reply = %+v", out)
	}
}

// TestInbound_CancelWithNothingRunning keeps the old wording when there is
// nothing to stop.
func TestInbound_CancelWithNothingRunning(t *testing.T) {
	al, msgBus, _ := newBlockingLoop(t, newTestConfig(t))
	dispatch(al, inbound("c1", "id1", "/cancel"))
	if got := nextOutbound(t, msgBus).Content; got != "No pending messages to cancel." {
		t.Fatalf("reply = %q", got)
	}
}

// TestInbound_PruneIdleSessions removes only entries nobody holds that have
// been idle past the TTL.
func TestInbound_PruneIdleSessions(t *testing.T) {
	al := &AgentLoop{}
	idle := al.acquireSession("idle")
	al.releaseSession(idle)
	held := al.acquireSession("held")
	fresh := al.acquireSession("fresh")
	al.releaseSession(fresh)
	al.sessionsMu.Lock()
	idle.lastUsed = time.Now().Add(-2 * time.Hour)
	held.lastUsed = time.Now().Add(-2 * time.Hour)
	al.sessionsMu.Unlock()

	if n := al.pruneIdleSessions(time.Now(), time.Hour); n != 1 {
		t.Fatalf("pruned %d, want 1 (only the idle unheld entry)", n)
	}
	if _, ok := al.sessions["held"]; !ok {
		t.Fatal("held entry was pruned")
	}
	if _, ok := al.sessions["fresh"]; !ok {
		t.Fatal("recently used entry was pruned")
	}
	al.releaseSession(held)
	if n := al.pruneIdleSessions(time.Now().Add(2*time.Hour), time.Hour); n != 2 {
		t.Fatalf("pruned %d after release, want 2", n)
	}
}

// TestDailySpend_AlertsOncePerDay: the alert fires when the day's total first
// reaches the threshold, not again that day, and again on the next day.
func TestDailySpend_AlertsOncePerDay(t *testing.T) {
	var s dailySpend
	day1 := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Hour)

	if _, _, crossed := s.add(0.6, 1.0, day1); crossed {
		t.Fatal("crossed below threshold")
	}
	if _, total, crossed := s.add(0.6, 1.0, day1); !crossed || total < 1.2 {
		t.Fatalf("crossed=%v total=%v, want first crossing at 1.2", crossed, total)
	}
	if _, _, crossed := s.add(5, 1.0, day1); crossed {
		t.Fatal("alerted twice in one day")
	}
	if day, total, crossed := s.add(1.0, 1.0, day2); !crossed || day != "2026-09-27" || total != 1.0 {
		t.Fatalf("day=%q total=%v crossed=%v, want a fresh day 2026-09-27 crossing at 1.0", day, total, crossed)
	}
	if _, _, crossed := s.add(1.0, 0, day2.Add(time.Hour)); crossed {
		t.Fatal("threshold 0 must never fire")
	}
}

// TestRecordSpend_RaisesNormalAlertOnce wires the tracker to the loop's alerter.
func TestRecordSpend_RaisesNormalAlertOnce(t *testing.T) {
	tl := newTestAgentLoop(t)
	tl.cfg.Agents.Defaults.DailySpendAlertUSD = 1.0
	rec := &alertRecorder{}
	tl.al.SetAlerter(rec)

	tl.al.recordSpend(0.5)
	tl.al.recordSpend(0.5)
	tl.al.recordSpend(0.5)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(rec.alerts))
	}
	a := rec.alerts[0]
	if a.Title != "Daily model spend over threshold" || !strings.HasPrefix(a.EventID, "spend:") || a.Priority != 0 {
		t.Fatalf("unexpected alert %+v", a)
	}
}

// A turn cancelled on purpose (its asker stopped waiting, or /cancel) is not a
// failed model call: it is logged at INFO, never as "LLM call failed".
func TestRunAgentLoop_DeliberateCancelNotLoggedAsFailure(t *testing.T) {
	for name, cause := range map[string]error{"asker stopped": errAskerStopped, "/cancel": errCancelledByUser} {
		t.Run(name, func(t *testing.T) {
			logs := &safeBufLoop{}
			t.Cleanup(logger.RedirectForTest(logs))
			provider := newBlockingProvider()
			cfg := newTestConfig(t)
			al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), provider, nil)
			agent := mustGetAgent(t, al)
			ctx, stop := context.WithCancelCause(context.Background())
			go func() {
				<-provider.started
				stop(cause)
			}()
			_, err := al.runAgentLoop(ctx, agent, processOptions{
				SessionKey: "agent:main:main", Channel: "cli", ChatID: "direct", UserMessage: "hi",
			})
			if !errors.Is(err, cause) {
				t.Fatalf("err = %v, want the cause %v", err, cause)
			}
			out := logs.String()
			if strings.Contains(out, "LLM call failed") {
				t.Fatalf("a deliberate cancellation was logged as a failure:\n%s", out)
			}
			if !strings.Contains(out, "LLM call stopped") {
				t.Fatalf("the stop was not logged:\n%s", out)
			}
		})
	}
}
