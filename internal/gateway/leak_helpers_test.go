// ClawEh
// License: MIT

package gateway

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// goroutinesIn counts the running goroutines whose stack names fn.
func goroutinesIn(fn string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, fn) {
			count++
		}
	}
	return count
}

// serviceGoroutines are the long-running loops the gateway's services start.
var serviceGoroutines = []string{
	"gateway.startBackupScheduler",
	"middleware.(*AuthStore).Watch",
	"cron.(*CronService).runLoop",
	"folderwatch.(*Watcher)",
	"gateway.(*httpHost).serve",
	"media.(*FileMediaStore)",
}

// countServiceGoroutines counts each of serviceGoroutines.
func countServiceGoroutines() map[string]int {
	out := map[string]int{}
	for _, fn := range serviceGoroutines {
		out[fn] = goroutinesIn(fn)
	}
	return out
}

// requireNoNewServiceGoroutines waits until no service loop runs beyond the
// counts taken in before, failing with the ones still running.
func requireNoNewServiceGoroutines(t *testing.T, before map[string]int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var left []string
		for _, fn := range serviceGoroutines {
			if n := goroutinesIn(fn); n > before[fn] {
				left = append(left, fn)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after the stop: %v", left)
		}
		time.Sleep(10 * time.Millisecond) // polling a condition, not timing
	}
}

// requirePortFree fails when the loopback port is still bound.
func requirePortFree(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d still bound: %v", port, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}
