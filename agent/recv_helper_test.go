// ClawEh
// License: MIT

package agent

import (
	"testing"
	"time"
)

// recv receives from ch, failing the test (rather than hanging it) when
// nothing comes within a generous bound.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}
