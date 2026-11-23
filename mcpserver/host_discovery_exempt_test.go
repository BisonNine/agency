// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"testing"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// regWithHiddenSuiteTool returns a registry holding one suite tool registered
// hidden behind discovery — exactly how the maestro/fusion suites register when
// progressive discovery is enabled (a suite-exempt entry from discovery.HideEntries).
func regWithHiddenSuiteTool(tool tools.Tool) *tools.ToolRegistry {
	r := tools.NewToolRegistry()
	r.RegisterEntries(hiddenSuiteEntry(tool))
	return r
}

// hiddenSuiteEntry is tool registered as a suite tool hidden by discovery.
func hiddenSuiteEntry(tool tools.Tool) tools.ToolEntry {
	e := discovery.HideEntries([]toolkit.Entry{{Tool: tool}})
	return tools.ToolEntry{Entry: e[0], SuiteExempt: true}
}

// TestHost_ListsAndDispatchesHiddenSuiteTool is the regression for the Alice
// bug: a claude-cli agent reaching a maestro tool through the MCP host got
// "tool_not_in_registry" because the tool was discovery-hidden. The host
// must never apply progressive discovery — it lists the tool AND dispatches it,
// authorization coming from the ACL policy, not the discovery state.
func TestHost_ListsAndDispatchesHiddenSuiteTool(t *testing.T) {
	tool := &mockTool{name: "maestro_file_list", params: map[string]any{}, result: tools.NewToolResult("ok")}
	reg := regWithHiddenSuiteTool(tool)

	// A hidden tool is registered like any other: discovery never gates it.
	if _, ok := reg.Get("maestro_file_list"); !ok {
		t.Fatal("Get must resolve a discovery-hidden tool")
	}

	regs := map[string]*tools.ToolRegistry{"alice": reg}
	st, tok := seedSessionToken("alice")
	resolver := resolverFor(regs)

	// (a) tools/list on the host includes the hidden suite tool.
	srv := hostFor(t, regs, st)
	for _, ep := range []struct{ path, tok string }{{InternalEndpointPath, ""}, {DefaultEndpointPath, tok}} {
		if _, ok := listTools(t, srv, ep.path, ep.tok)["maestro_file_list"]; !ok {
			t.Fatalf("%s must list the discovery-hidden suite tool", ep.path)
		}
	}

	// (b) dispatch succeeds instead of returning tool_not_in_registry.
	out, isErr := dispatchToolCall(context.Background(), "maestro_file_list",
		map[string]any{"session_token": tok}, st, resolver, nil, acl.Default, nil, nil)
	if isErr {
		t.Fatalf("host dispatch of a hidden suite tool must succeed, got error: %s", out)
	}
	if tool.calls != 1 {
		t.Fatalf("expected the tool to execute once, got %d calls", tool.calls)
	}
}
