// ClawEh
// License: MIT

package agent

import (
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := &mockCustomTool{}
			named := namedTool{Tool: tool, name: c.tool}
			e := discovery.HideEntries([]toolkit.Entry{{Tool: named}}, alwaysShown(c.alwaysShown))[0]
			if e.Hidden != c.wantHidden {
				t.Fatalf("pins %v, tool %q: hidden = %v, want %v", c.alwaysShown, c.tool, e.Hidden, c.wantHidden)
			}
		})
	}
}

// namedTool is a tool under a given internal name.
type namedTool struct {
	tools.Tool
	name string
}

func (n namedTool) Name() string { return n.name }
