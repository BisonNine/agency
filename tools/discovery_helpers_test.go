// ClawEh
// License: MIT

package tools

import (
	"context"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"
)

// hiddenEntry is tool's entry hidden by discovery, as the agent registers a
// discovery-eligible tool: in group, loaded with it when revealTogether.
func hiddenEntry(tool Tool, group string, revealTogether bool) ToolEntry {
	e := discovery.HideEntries([]toolkit.Entry{{Tool: tool, Group: group, RevealTogether: revealTogether}})
	return ToolEntry{Entry: e[0]}
}

// suiteEntry is e suite-exempt.
func suiteEntry(e ToolEntry) ToolEntry {
	e.SuiteExempt = true
	return e
}

// listedEntry is tool's entry, listed.
func listedEntry(tool Tool) ToolEntry {
	return ToolEntry{Tool: tool}
}

// registerHidden registers each tool hidden by discovery, in no group.
func registerHidden(r *ToolRegistry, ts ...Tool) {
	for _, t := range ts {
		r.RegisterEntries(hiddenEntry(t, "", false))
	}
}

// metaTool calls one of a registry's discovery meta-tools by name, through
// the registry.
type metaTool struct {
	r    *ToolRegistry
	name string
}

func (m metaTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return m.r.Execute(ctx, m.name, args)
}

func (m metaTool) Description() string {
	t, ok := m.r.Get(m.name)
	if !ok {
		return ""
	}
	return t.Description()
}

// discoveryTools registers r's discovery meta-tools with at most maxResults
// hits per search and returns them.
func discoveryTools(r *ToolRegistry, maxResults int) (search, details metaTool) {
	r.RegisterDiscoveryTools(context.Background(), maxResults, nil)
	return metaTool{r, discovery.SearchToolName}, metaTool{r, discovery.DetailsToolName}
}
