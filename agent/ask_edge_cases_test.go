// Agency
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
)

// An ask nobody waits for has no asker.
func TestAsk_WaitUnknown(t *testing.T) {
	var r askRegistry
	if _, ok := r.wait("ask-none"); ok {
		t.Fatal("an unknown ask has an asker")
	}
}

// Each wait of a caller on a target is ended once; a cycle elsewhere in the
// graph does not refuse an unrelated wait.
func TestAsk_WaitGraph(t *testing.T) {
	var g waitGraph
	for range 2 {
		if !g.begin("alice", "bob") {
			t.Fatal("alice could not wait on bob twice")
		}
	}
	g.end("alice", "bob")
	if g.begin("bob", "alice") {
		t.Fatal("bob may wait on alice while alice still waits on bob")
	}
	g.end("alice", "bob")
	if !g.begin("bob", "alice") {
		t.Fatal("bob may not wait on alice once alice's waits ended")
	}

	var cyc waitGraph
	cyc.edges = map[string]map[string]int{"a": {"b": 1}, "b": {"a": 1}}
	if !cyc.begin("c", "a") {
		t.Fatal("a cycle between a and b refused c waiting on a")
	}
}

// Without a configuration no request timeout applies.
func TestAsk_RequestTimeoutWithoutConfig(t *testing.T) {
	if got := (&AgentLoop{}).requestTimeoutFor(&AgentInstance{ID: "alice"}); got != 0 {
		t.Fatalf("requestTimeoutFor = %s, want 0", got)
	}
}

// An ask with no sender, no wait, or while the service is not running is
// refused; one the bus refuses to queue is an error.
func TestAsk_RefusedWithoutSenderOrService(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus := hooksEdgeLoop(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name, from string
		wait       time.Duration
		want       string
	}{
		{"no sender", " ", time.Minute, "ask: the sender is required"},
		{"no wait", "Alice", 0, "ask: the wait must be positive"},
		{"not running", "Alice", time.Minute, "the service is not running"},
	} {
		if _, err := al.Ask(ctx, tt.from, "main", "hello", tt.wait); err == nil || err.Error() != tt.want {
			t.Fatalf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}

	al.running.Store(true)
	defer al.running.Store(false)
	msgBus.Close()
	_, err := al.Ask(ctx, "Alice", "main", "hello", time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "queue the message: ") || !strings.Contains(err.Error(), bus.ErrBusClosed.Error()) {
		t.Fatalf("err = %v, want the queueing error", err)
	}
	al.asks.mu.Lock()
	defer al.asks.mu.Unlock()
	if len(al.asks.waiting) != 0 {
		t.Fatalf("asks left waiting: %v", al.asks.waiting)
	}
}
