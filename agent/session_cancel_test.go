// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"iter"
	"testing"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
)

// probeRun is a hook.Run that records how it was stopped.
type probeRun struct {
	done     chan struct{}
	onCancel func(cause error)
}

func (r *probeRun) Events() iter.Seq2[hook.Event, error] {
	return func(func(hook.Event, error) bool) {}
}
func (r *probeRun) Interrupt()            { r.Cancel(hook.ErrInterrupted) }
func (r *probeRun) Cancel(cause error)    { r.onCancel(cause) }
func (r *probeRun) Done() <-chan struct{} { return r.done }
func (r *probeRun) Wait() hook.TurnResult { <-r.done; return hook.TurnResult{} }

// A /cancel sets the turn's own cause before anything else of the cancel
// runs: when the stopped run ends at once, its outcome is decided with the
// cause in place (never as an error reply). The run is then stopped with
// the same cause.
func TestSessionCancel_CauseSetBeforeTheEngineStops(t *testing.T) {
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	var causeAtStop, causeGiven error
	run := &probeRun{done: make(chan struct{})}
	run.onCancel = func(c error) {
		causeAtStop, causeGiven = context.Cause(turnCtx), c
		close(run.done) // as if the run finished at once
	}
	al := &AgentLoop{cfg: &config.Config{}}
	alice := &AgentInstance{ID: "alice"}
	_, ht := al.withHostTurn(context.Background(), alice, processOptions{SessionKey: "s"}, nil, nil, nil)
	h := &runHandle{al: al, agent: alice, run: run, turn: ht}
	if !h.running() {
		t.Fatal("a run not yet finished does not read as running")
	}
	hs := &turnHandles{h: h, claimed: true}
	ss := &sessionState{turnCancel: stopTurn, handles: hs}

	ss.cancel(context.Background(), &userCancelError{chat: "test::c1"})
	if !errors.Is(causeAtStop, errCancelledByUser) || !errors.Is(causeGiven, errCancelledByUser) {
		t.Fatalf("cause when the run was stopped = %v, given to it = %v; want the /cancel's", causeAtStop, causeGiven)
	}
	if h.running() {
		t.Fatal("the stopped run still reads as running")
	}
}
