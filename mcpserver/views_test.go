// Agency
// License: MIT

package mcpserver

import (
	"runtime"
	"testing"
	"time"

	"go.bison9.com/agency/tools"
)

// An agent's view is built once per endpoint and dropped once its registry
// is gone (a deleted temporary agent), so the cache never holds one alive.
func TestEndpointViews_ReusedAndDropped(t *testing.T) {
	srv := hostFor(t, map[string]*tools.ToolRegistry{"alice": newRegistryWith(&mockTool{name: "a", params: map[string]any{}})}, NewSessionTokenStore())
	h := newEndpointHost(srv, false, []string{"*"})
	func() {
		reg := newRegistryWith(&mockTool{name: "b", params: map[string]any{}})
		first := h.view("clone", reg)
		if h.view("clone", reg) != first {
			t.Error("the same agent's view of one registry was built twice")
		}
		if h.view("clone", reg) == h.view("other", reg) {
			t.Error("two agents share one view")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		h.viewsMu.Lock()
		n := len(h.views)
		h.viewsMu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d views kept after their registry was gone", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
