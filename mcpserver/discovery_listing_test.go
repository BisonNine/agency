// Agency
// License: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/tools"
)

// The MCP host publishes every granted tool with discovery on, the
// discovery meta-tools included, under the descriptions their tools report:
// search_tools names the hidden namespaces of the first agent's registry.
// The fixture was captured before discovery moved to the SDK's discovery
// package and must not change.
func TestHost_DiscoveryListingUnchanged(t *testing.T) {
	regs := map[string]*tools.ToolRegistry{
		"alice": discoveryListingRegistry([]string{"mcp_alpha_read", "mcp_alpha_write", "fusion_svc_list"}, []string{"time_now"}),
		"bob":   discoveryListingRegistry([]string{"mcp_bob_fetch"}, []string{"file_read"}),
	}
	st, _ := seedSessionToken("alice")
	listed := listTools(t, hostFor(t, regs, st), InternalEndpointPath, "")
	names := make([]string, 0, len(listed))
	for n := range listed {
		names = append(names, n)
	}
	sort.Strings(names)
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]entry, 0, len(names))
	for _, n := range names {
		out = append(out, entry{n, listed[n].Tool.Description})
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "discovery_listing.json")
	if os.Getenv("UPDATE_TOOLLIST") == "1" {
		if werr := os.WriteFile(path, got, 0o644); werr != nil {
			t.Fatal(werr)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("MCP host listing differs from %s:\n got %s", path, got)
	}
}

// discoveryListingRegistry is an agent's registry with discovery on: listed
// tools, hidden tools and the two meta-tools.
func discoveryListingRegistry(hidden, listed []string) *tools.ToolRegistry {
	r := tools.NewToolRegistry()
	for _, n := range listed {
		r.Register(&mockTool{name: n, params: map[string]any{}, result: toolkit.NewResult("ok")})
	}
	for _, n := range hidden {
		registerHiddenForListing(r, &mockTool{name: n, params: map[string]any{}, result: toolkit.NewResult("ok")})
	}
	registerDiscoveryForListing(r)
	return r
}

// registerHiddenForListing registers t hidden by discovery.
func registerHiddenForListing(r *tools.ToolRegistry, t toolkit.Tool) {
	e := discovery.HideEntries([]toolkit.Entry{{Tool: t}})
	r.RegisterEntries(tools.ToolEntry{Entry: e[0]})
}

// registerDiscoveryForListing registers the discovery meta-tools.
func registerDiscoveryForListing(r *tools.ToolRegistry) {
	r.RegisterDiscoveryTools(context.Background(), 10, nil)
}
