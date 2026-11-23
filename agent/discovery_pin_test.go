// ClawEh
// License: MIT

package agent

import (
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/tools"
)

// TestAlwaysShown covers the in-loop pinning decision with discovery on: a
// discovery-eligible tool stays listed only when its internal name matches a
// pin (always_shown_namespaces, matched with MatchVisibility, so mcp_ is
// stripped and a namespace prefix matches). Discovery off hides nothing and
// never asks.
func TestAlwaysShown(t *testing.T) {
	cases := []struct {
		name        string
		alwaysShown []string
		tool        string
		wantHidden  bool
	}{
		{"nothing pinned: hidden", nil, "maestro_file_list", true},
		{"namespace pinned: listed", []string{"maestro"}, "maestro_file_list", false},
		{"other namespace pinned: hidden", []string{"fusion"}, "maestro_file_list", true},
		{"star pins everything: listed", []string{"*"}, "maestro_file_list", false},
		{"pin strips mcp_ prefix for upstream tools", []string{"trello"}, "mcp_trello_search", false},
		{"pin does not match a different mcp server", []string{"trello"}, "mcp_github_search", true},
		{"internal name only: server named mcp_alpha", []string{"alpha"}, "mcp_mcp_alpha_scan", true},
		{"internal name only: external name not asked", []string{"alpha_scan"}, "mcp_mcp_alpha_scan", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			named := namedTool{Tool: &mockCustomTool{}, name: c.tool}
			e := discovery.HideEntries([]toolkit.Entry{{Tool: named}}, alwaysShown(c.alwaysShown))[0]
			if e.Hidden != c.wantHidden {
				t.Fatalf("pins %v, tool %q: hidden = %v, want %v", c.alwaysShown, c.tool, e.Hidden, c.wantHidden)
			}
		})
	}
}

// namedTool is a tool under a given internal name, offered (like an upstream
// MCP tool) under that name without its mcp_ prefix.
type namedTool struct {
	toolkit.Tool
	name string
}

func (n namedTool) Name() string { return n.name }

func (n namedTool) ExternalName() string { return strings.TrimPrefix(n.name, "mcp_") }

// A hidden tool of a reveal-together MCP server is grouped by its server, so
// get_tool_details on one references the server's other hidden tools; a
// pinned one is listed with no group.
func TestHideMCPEntries_RevealTogether(t *testing.T) {
	names := []string{"mcp_alpha_read", "mcp_alpha_write", "mcp_alpha_list"}
	eligible := make([]toolkit.Entry, 0, len(names))
	for _, n := range names {
		eligible = append(eligible, toolkit.Entry{Tool: namedTool{Tool: &mockCustomTool{}, name: n}})
	}
	r := tools.NewToolRegistry()
	r.RegisterEntries(hideMCPEntries(eligible, []string{"alpha_list"}, "alpha", true)...)
	r.RegisterDiscoveryTools(t.Context(), 10, nil)

	res := r.Execute(t.Context(), "get_tool_details", map[string]any{"name": "mcp_alpha_read"})
	if want := []string{"alpha_read", "alpha_write"}; !slices.Equal(res.ToolReferences, want) {
		t.Errorf("references = %v, want %v", res.ToolReferences, want)
	}
	res = r.Execute(t.Context(), "get_tool_details", map[string]any{"name": "mcp_alpha_list"})
	if len(res.ToolReferences) != 0 {
		t.Errorf("pinned tool: references = %v, want none", res.ToolReferences)
	}
}
