// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agent/hook"
)

// A /cancel sets the turn's own cause before anything else of the cancel
// runs: when the stopped engine turn ends at once, its outcome is decided
// with the cause in place (never as an error reply). The interrupt
// observers run after it, told a running turn was stopped.
func TestSessionCancel_CauseSetBeforeTheEngineStops(t *testing.T) {
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	engineCtx, stopEngine := context.WithCancelCause(context.Background()) // as if the engine finished on its own context
	var causeAtStop, causeAtObserver error
	var running bool
	hooks := &turnHooks{}
	hooks.add(hook.Plugin{Name: "probe", OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
		causeAtObserver, running = context.Cause(turnCtx), ev.Running
	}})
	h := &turnHandle{ctx: engineCtx, done: make(chan struct{}), hooks: hooks, cancel: func(c error) {
		causeAtStop = context.Cause(turnCtx)
		stopEngine(c)
	}}
	hs := &turnHandles{h: h, claimed: true}
	ss := &sessionState{turnCancel: stopTurn, handles: hs}

	ss.cancel(context.Background(), &userCancelError{chat: "test::c1"})
	if !errors.Is(causeAtStop, errCancelledByUser) || !errors.Is(causeAtObserver, errCancelledByUser) {
		t.Fatalf("cause when the engine was stopped = %v, at the observers = %v; want the /cancel's", causeAtStop, causeAtObserver)
	}
	if !running {
		t.Fatal("the observers were not told a running turn was stopped")
	}
}
