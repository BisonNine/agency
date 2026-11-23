package tools

import (
	"slices"
	"testing"
)

// TestReplaceByPrefix: the prefix's old tools are swapped for the new entries
// (visible and hidden kept as given), unrelated tools stay, and the version
// moves so cached definitions rebuild.
func TestReplaceByPrefix(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{name: "mcp_alice_search"})
	registerHidden(r, &mockRegistryTool{name: "mcp_alice_fetch"})
	r.Register(&mockRegistryTool{name: "mcp_bob_search"})

	before := r.Version()
	removed := r.ReplaceByPrefix("mcp_alice_", []ToolEntry{
		listedEntry(&mockRegistryTool{name: "mcp_alice_search"}),
		hiddenEntry(&mockRegistryTool{name: "mcp_alice_lookup"}, "alice", true),
	})
	if removed != 2 {
		t.Fatalf("ReplaceByPrefix removed %d, want 2", removed)
	}
	if r.Version() == before {
		t.Fatal("ReplaceByPrefix must bump the registry version")
	}
	want := []string{"mcp_alice_lookup", "mcp_alice_search", "mcp_bob_search"}
	if got := r.List(); !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	deferred := make(map[string]bool)
	for _, d := range r.ToolDefs(nil).Send {
		deferred[d.Function.Name] = d.Deferred
	}
	if v, ok := deferred["mcp_alice_search"]; !ok || v {
		t.Error("mcp_alice_search should be offered as a core tool")
	}
	if v, ok := deferred["mcp_alice_lookup"]; !ok || !v {
		t.Error("mcp_alice_lookup was registered hidden and should be offered deferred")
	}

	// Nothing under the prefix and nothing new: no change, no version bump.
	r.ReplaceByPrefix("mcp_carol_", nil)
	after := r.Version()
	if r.ReplaceByPrefix("mcp_carol_", nil) != 0 || r.Version() != after {
		t.Fatal("an empty replacement must remove nothing and leave the version alone")
	}
}
