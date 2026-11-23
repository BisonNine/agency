// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agency/tools"
)

// Two agents that hold tools of the same name: each agent's client is served
// its own agent's tool, never the other's data or description. On /mcp the
// bearer names the agent, so tools/list describes the caller's own tools
// (search_tools names the caller's own hidden namespaces); /internal lists
// with no caller, so it shows the union, described by the first agent.
func TestSameNamedTools_EachAgentGetsItsOwn(t *testing.T) {
	notes := func(owner string) *wireTool {
		return &wireTool{name: "notes_read", desc: owner + "'s notes", params: map[string]any{}, run: wireText(owner + "-secret")}
	}
	alice := discoveryListingRegistry([]string{"alpha_read"}, nil)
	alice.Register(notes("alice"))
	bob := discoveryListingRegistry([]string{"beta_fetch"}, nil)
	bob.Register(notes("bob"))
	regs := map[string]*tools.ToolRegistry{"alice": alice, "bob": bob}

	st := NewSessionTokenStore()
	aliceTok := st.Issue("alice", "agent:alice:main", "/tmp/a")
	bobTok := st.Issue("bob", "agent:bob:main", "/tmp/b")
	srv := hostFor(t, regs, st)

	desc := func(path, bearer, tool string) string {
		lt, ok := listTools(t, srv, path, bearer)[tool]
		if !ok {
			t.Fatalf("%s not listed on %s", tool, path)
		}
		return lt.Tool.Description
	}
	if got := desc(DefaultEndpointPath, bobTok, "notes_read"); got != "bob's notes" {
		t.Errorf("/mcp for Bob describes notes_read as %q, want Bob's", got)
	}
	if got := desc(DefaultEndpointPath, aliceTok, "notes_read"); got != "alice's notes" {
		t.Errorf("/mcp for Alice describes notes_read as %q, want Alice's", got)
	}
	if got := desc(InternalEndpointPath, "", "notes_read"); got != "alice's notes" {
		t.Errorf("/internal describes notes_read as %q, want the first agent's", got)
	}
	bobSearch := desc(DefaultEndpointPath, bobTok, "search_tools")
	if !strings.Contains(bobSearch, "beta") || strings.Contains(bobSearch, "alpha") {
		t.Errorf("/mcp search_tools for Bob names another agent's namespaces: %q", bobSearch)
	}
	if internal := desc(InternalEndpointPath, "", "search_tools"); !strings.Contains(internal, "alpha") {
		t.Errorf("/internal search_tools = %q, want the first agent's", internal)
	}

	for _, tc := range []struct{ tok, want string }{{aliceTok, "alice-secret"}, {bobTok, "bob-secret"}} {
		if out, _ := callOn(context.Background(), t, srv, "notes_read", map[string]any{"session_token": tc.tok}); out != tc.want {
			t.Errorf("/internal notes_read = %q, want %q", out, tc.want)
		}
		code, raw := rpc(context.Background(), srv.httpServer.Handler, DefaultEndpointPath, tc.tok, "tools/call",
			map[string]any{"name": "notes_read", "arguments": map[string]any{}})
		if code != 200 || !strings.Contains(string(raw), `"`+tc.want+`"`) || strings.Count(string(raw), "-secret") != 1 {
			t.Errorf("/mcp notes_read: HTTP %d %s, want only %q", code, raw, tc.want)
		}
	}
}
