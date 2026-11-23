// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/logger"
)

const busClosedLog = "Message bus closed; the agent loop waits for shutdown"

// A bus closed under a running loop delivers nothing more: Run stops
// consuming (it notes the close once instead of polling the closed bus) and
// keeps running until Stop, so the shutdown order — Stop, then WaitTurns —
// still holds, and it then returns nil.
func TestRun_ClosedBusWaitsForStop(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	tl := newTestAgentLoop(t)
	al := tl.al

	runErr := make(chan error, 1)
	go func() { runErr <- al.Run(t.Context()) }()
	awaitSignal(t, al.Started(), "Run to start")
	tl.msgBus.Close()

	deadline := time.After(30 * time.Second)
	for !strings.Contains(logs.String(), busClosedLog) {
		select {
		case err := <-runErr:
			t.Fatalf("Run returned %v before Stop", err)
		case <-deadline:
			t.Fatal("Run did not notice the closed bus")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-runErr:
		t.Fatalf("Run returned %v before Stop", err)
	default:
	}

	al.Stop()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil after Stop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if n := strings.Count(logs.String(), busClosedLog); n != 1 {
		t.Errorf("closed-bus note logged %d times, want once", n)
	}
	if err := al.WaitTurns(context.Background()); err != nil {
		t.Fatal(err)
	}
	al.Close(context.Background())
}
