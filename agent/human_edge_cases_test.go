// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
)

// humanEdgeAsk is an asker that waits until deadline, with a gone channel the
// test controls.
func humanEdgeAsk(deadline time.Time) (*askWait, chan struct{}) {
	gone := make(chan struct{})
	return &askWait{from: "Alice", deadline: deadline, gone: gone}, gone
}

// A request whose asker has stopped, or whose turn has ended, while another
// request holds the person is never posted.
func TestHuman_QueuedRequestEndsBeforePosting(t *testing.T) {
	for _, tt := range []struct {
		name    string
		blocked bool // the end comes while the request already waits for the person
		asker   bool // the asker stops (else the turn's ctx ends)
		want    error
	}{
		{"asker gone before queueing", false, true, errAskerStopped},
		{"turn ended before queueing", false, false, context.Canceled},
		{"asker gone while queued", true, true, errAskerStopped},
		{"turn ended while queued", true, false, context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
			al, msgBus, _ := newHumanLoop(t, 60)
			var queued <-chan struct{}
			al.onWait, queued = waitSignals(waitPerson)
			al.humans.slot("bob") <- struct{}{} // another request holds the person

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ask *askWait
			var gone chan struct{}
			if tt.asker {
				ask, gone = humanEdgeAsk(al.clk().Now().Add(time.Minute))
			}
			stop := func() {
				if tt.asker {
					close(gone)
				} else {
					cancel()
				}
			}
			if !tt.blocked {
				stop()
			}
			done := make(chan error, 1)
			go func() {
				_, err := al.askHuman(ctx, "bob", "test", bobChat, "question", time.Minute, ask)
				done <- err
			}()
			if tt.blocked {
				awaitSignal(t, queued, "the request to queue")
				stop()
			}
			if err := recv(t, done, "askHuman"); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			noOutbound(t, msgBus)
		})
	}
}

// An asker whose deadline has already passed when the person is free gets
// no request posted.
func TestHuman_AskerDeadlinePassed(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus, _ := newHumanLoop(t, 60)
	ask, _ := humanEdgeAsk(al.clk().Now().Add(-time.Second))
	if _, err := al.askHuman(context.Background(), "bob", "test", bobChat, "question", time.Minute, ask); !errors.Is(err, errAskerStopped) {
		t.Fatalf("err = %v, want errAskerStopped", err)
	}
	noOutbound(t, msgBus)
	if al.humans.waiting("bob") {
		t.Fatal("a request is left waiting")
	}
}

// A request the bus refuses to take is an error, and nothing is left waiting.
func TestHuman_PostFails(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus, _ := newHumanLoop(t, 60)
	msgBus.Close()
	_, err := al.askHuman(context.Background(), "bob", "test", bobChat, "question", time.Minute, nil)
	if !errors.Is(err, bus.ErrBusClosed) || !strings.Contains(err.Error(), "test:"+bobChat) {
		t.Fatalf("err = %v, want the bus error naming the chat", err)
	}
	if al.humans.waiting("bob") {
		t.Fatal("a request is left waiting")
	}
}

// An asker that stops waiting before its deadline withdraws the posted
// request: the person is told who no longer needs it.
func TestHuman_AskerWithdraws(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus, _ := newHumanLoop(t, 60)
	ask, gone := humanEdgeAsk(al.clk().Now().Add(time.Minute))
	done := make(chan error, 1)
	go func() {
		_, err := al.askHuman(context.Background(), "bob", "test", bobChat, "question", 0, ask)
		done <- err
	}()
	expectPosted(t, msgBus)
	close(gone)
	if err := recv(t, done, "askHuman"); !errors.Is(err, errAskerStopped) {
		t.Fatalf("err = %v, want errAskerStopped", err)
	}
	expectInBobChat(t, msgBus, "Alice no longer needs an answer to that request.")
	deliver(al, fromBob("b1", "Late answer"))
	expectInBobChat(t, msgBus, withdrawnReply)
}

// A turn that ends with no asker withdraws its request without naming one.
func TestHuman_TurnEndsWithoutAsker(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus, _ := newHumanLoop(t, 60)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := al.askHuman(ctx, "bob", "test", bobChat, "question", 0, nil)
		done <- err
	}()
	expectPosted(t, msgBus)
	cancel()
	if err := recv(t, done, "askHuman"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	expectInBobChat(t, msgBus, "That request is no longer needed.")
}

// The person's window is cut to the asker's deadline; running out is the
// asker stopping, not the request's own timeout.
func TestHuman_WindowCappedByAsker(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus, _ := newHumanLoop(t, 60)
	fc := fakeClock(al)
	ask, _ := humanEdgeAsk(fc.Now().Add(time.Minute))
	done := make(chan error, 1)
	go func() {
		_, err := al.askHuman(context.Background(), "bob", "test", bobChat, "question", time.Hour, ask)
		done <- err
	}()
	expectPosted(t, msgBus)
	advanceWhenWaiting(t, fc, 1, time.Minute)
	if err := recv(t, done, "askHuman"); !errors.Is(err, errAskerStopped) {
		t.Fatalf("err = %v, want errAskerStopped", err)
	}
	noOutbound(t, msgBus) // a timeout is not a withdrawal
}

// Telling the person a request is withdrawn survives a bus that refuses it.
func TestHuman_TellNoLongerNeededPublishFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus, _ := newHumanLoop(t, 60)
	msgBus.Close()
	al.tellNoLongerNeeded(context.Background(), "bob", "test", bobChat, "Alice")
	if !strings.Contains(logs.String(), "Failed to tell the person a request is no longer needed") {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}
}

// A reply in a person's chat that the bus refuses is logged.
func TestHuman_ReplyInHumanChatPublishFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus, _ := newHumanLoop(t, 60)
	msgBus.Close()
	al.replyInHumanChat(context.Background(), fromBob("b1", "x"), nothingWaitingReply)
	if !strings.Contains(logs.String(), "Failed to publish reply") {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}
}

// An unused answer for a human agent with no chat says nothing.
func TestHuman_AnswerUnusedWithoutChat(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := humanLoopConfig(t, 60)
	cfg.Bindings = nil
	al, msgBus, _ := newHumanLoopWith(t, cfg)
	if bob, ok := al.GetRegistry().Get("bob"); !ok || bob.HumanModel == "" {
		t.Fatal("bob is not a running human agent")
	}
	al.humanAnswerUnused(context.Background(), askBob("r1", "x"))
	noOutbound(t, msgBus)
}

// A loop with no configuration or registry knows no person's chat.
func TestHuman_ChatOwnerWithoutConfig(t *testing.T) {
	if _, ok := (&AgentLoop{}).humanChatOwner(fromBob("b1", "x")); ok {
		t.Fatal("a chat owner was found with no configuration")
	}
}

// The turn budget of a request to a person covers the person's wait plus
// the grace when that is longer than the base budget.
func TestHuman_TurnBudget(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _, _ := newHumanLoop(t, 600)
	want := 600*time.Second + humanTurnGrace
	if got := al.humanTurnBudget(askBob("r1", "x"), time.Minute); got != want {
		t.Fatalf("budget = %s, want %s", got, want)
	}
	if got := al.humanTurnBudget(askBob("r1", "x"), want+time.Hour); got != want+time.Hour {
		t.Fatalf("budget = %s, want the longer base", got)
	}
	if got := al.humanTurnBudget(inbound("c2", "a1", "Hi"), time.Minute); got != time.Minute {
		t.Fatalf("budget for an ordinary agent = %s, want the base", got)
	}
}

// humanEdgeOpts is a turn of bob answering ask id.
func humanEdgeOpts(id string) processOptions {
	return processOptions{
		SessionKey: routing.BuildAgentMainSessionKey("bob"), Channel: constants.AgentMessageChannel,
		ChatID: "ask-" + id, UserMessage: "question", ReplyRequired: true,
	}
}

// A person's turn whose asker is gone, whose agent has no chat or whose
// model is not a person's ends at once with an error.
func TestHuman_TurnRefusals(t *testing.T) {
	t.Run("asker gone", func(t *testing.T) {
		t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
		al, _, _ := newHumanLoop(t, 60)
		bob, _ := al.GetRegistry().Get("bob")
		if _, err := al.runHumanTurn(context.Background(), bob, humanEdgeOpts("r1")); !errors.Is(err, errAskerStopped) {
			t.Fatalf("err = %v, want errAskerStopped", err)
		}
	})
	t.Run("no chat", func(t *testing.T) {
		t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
		cfg := humanLoopConfig(t, 60)
		cfg.Bindings = nil
		al, _, _ := newHumanLoopWith(t, cfg)
		bob, _ := al.GetRegistry().Get("bob")
		al.asks.open("ask-r1", "Alice", al.clk().Now().Add(time.Minute), false)
		_, err := al.runHumanTurn(context.Background(), bob, humanEdgeOpts("r1"))
		if err == nil || err.Error() != "Bob has no chat to reach the person in" {
			t.Fatalf("err = %v, want no chat", err)
		}
	})
	for _, tt := range []struct {
		name  string
		model func(al *AgentLoop) string
	}{
		{"unknown model", func(*AgentLoop) string { return "no-such-model" }},
		{"not a person's model", func(al *AgentLoop) string {
			alice, _ := al.GetRegistry().Get("alice")
			return al.activeModelName(alice, routing.BuildAgentMainSessionKey("alice"))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
			al, msgBus, _ := newHumanLoop(t, 60)
			bob, _ := al.GetRegistry().Get("bob")
			bob.HumanModel = tt.model(al)
			al.asks.open("ask-r1", "Alice", al.clk().Now().Add(time.Minute), false)
			_, err := al.runHumanTurn(context.Background(), bob, humanEdgeOpts("r1"))
			if err == nil || err.Error() != `model "`+bob.HumanModel+`" is not a person's model` {
				t.Fatalf("err = %v, want not a person's model", err)
			}
			noOutbound(t, msgBus)
		})
	}
}

// A person's answer is published to the asking chat when the turn sends its
// response (a bus that refuses it is logged), and counts as one iteration.
func TestHuman_TurnSendsAnswer(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus, _ := newHumanLoop(t, 60)
	bob, _ := al.GetRegistry().Get("bob")
	al.asks.open("ask-r1", "Alice", al.clk().Now().Add(time.Minute), false)
	iterations := 0
	opts := humanEdgeOpts("r1")
	opts.SendResponse = true
	opts.IterationsOut = &iterations
	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		a, err := al.runHumanTurn(context.Background(), bob, opts)
		done <- result{a, err}
	}()
	expectPosted(t, msgBus)
	msgBus.Close()
	if !al.takeHumanAnswer(context.Background(), fromBob("b1", "Yes.")) {
		t.Fatal("answer not taken")
	}
	r := recv(t, done, "runHumanTurn")
	if r.err != nil || r.answer != "Yes." {
		t.Fatalf("runHumanTurn = %q, %v; want the answer", r.answer, r.err)
	}
	if iterations != 1 {
		t.Fatalf("iterations = %d, want 1", iterations)
	}
	if !strings.Contains(logs.String(), "Failed to publish response") {
		t.Fatalf("the refused publish was not logged: %s", logs.String())
	}
}
