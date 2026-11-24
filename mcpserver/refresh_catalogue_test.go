// Agency
// License: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/tools"
)

// RefreshCatalogue adds a tool that appeared in an agent registry, deletes one
// that disappeared, and replaces one whose definition changed — on both
// endpoints, as seen through the mcp-go server's tools/list.
func TestRefreshCatalogue_FollowsRegistryChanges(t *testing.T) {
	readFile := &mockTool{name: "read_file", desc: "v1", params: map[string]any{}, result: toolkit.NewResult("a")}
	writeFile := &mockTool{name: "write_file", params: map[string]any{}, result: toolkit.NewResult("a")}
	reg := newRegistryWith(readFile, writeFile)

	srv, err := New(
		WithAgentRegistries(map[string]*tools.ToolRegistry{"alice": reg}),
		WithAllowlist([]string{"*"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listedToolNames(t, srv, nil), []string{"read_file", "write_file"}; !slices.Equal(got, want) {
		t.Fatalf("initial catalogue = %v, want %v", got, want)
	}

	// A tool appears, one disappears, and one is re-registered with a new
	// description (the way an unchanged upstream MCP tool is refreshed).
	reg.Register(&mockTool{name: "list_dir", params: map[string]any{}, result: toolkit.NewResult("a")})
	reg.ReplaceByPrefix("write_file", nil)
	reg.Register(&mockTool{name: "read_file", desc: "v2", params: map[string]any{}, result: toolkit.NewResult("a")})

	srv.RefreshCatalogue()

	if got, want := listedToolNames(t, srv, nil), []string{"list_dir", "read_file"}; !slices.Equal(got, want) {
		t.Fatalf("catalogue after refresh = %v, want %v", got, want)
	}
	if desc := listTools(t, srv, InternalEndpointPath, "")["read_file"].Tool.Description; desc != "v2" {
		t.Errorf("read_file description after refresh = %q, want v2", desc)
	}

	// The bearer endpoint follows the same registries.
	st, tok := seedSessionToken("alice")
	srv.sessionTokens = st
	listed := listTools(t, srv, DefaultEndpointPath, tok)
	if _, ok := listed["list_dir"]; !ok {
		t.Error("bearer endpoint should publish the added tool")
	}
	if _, ok := listed["write_file"]; ok {
		t.Error("bearer endpoint should no longer publish the removed tool")
	}

	// A refresh with nothing changed publishes nothing new on either endpoint.
	if srv.internal.Refresh(context.Background()).Changed || srv.bearer.Refresh(context.Background()).Changed {
		t.Error("a no-op refresh reported a change")
	}
}

// Each endpoint logs, at debug level, a "registered tool" line per tool it
// registers (at start and on a refresh) and a "removed tools" line naming
// what a refresh withdrew.
func TestRefreshCatalogue_LogsRegisteredAndRemoved(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	reg := newRegistryWith(
		&mockTool{name: "read_file", params: map[string]any{}, result: toolkit.NewResult("a")},
		&mockTool{name: "write_file", params: map[string]any{}, result: toolkit.NewResult("a")},
	)
	srv, err := New(
		WithAgentRegistries(map[string]*tools.ToolRegistry{"alice": reg}),
		WithAllowlist([]string{"*"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		Level   string   `json:"level"`
		Caller  string   `json:"caller"`
		Message string   `json:"message"`
		Tool    string   `json:"tool"`
		Tools   []string `json:"tools"`
	}
	lines := func() []line {
		var out []line
		for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			var e line
			if json.Unmarshal([]byte(l), &e) == nil && (e.Message == "registered tool" || e.Message == "removed tools") {
				// The caller field starts with the component.
				e.Caller = strings.Fields(e.Caller)[0]
				out = append(out, e)
			}
		}
		buf.Reset()
		return out
	}
	// Two endpoints, two tools each.
	registered := line{Level: "debug", Caller: "mcpserver", Message: "registered tool"}
	got := lines()
	want := []line{registered, registered, registered, registered}
	want[0].Tool, want[1].Tool, want[2].Tool, want[3].Tool = "read_file", "write_file", "read_file", "write_file"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines at start = %+v, want %+v", got, want)
	}

	reg.ReplaceByPrefix("write_file", nil)
	srv.RefreshCatalogue()
	removed := line{Level: "debug", Caller: "mcpserver", Message: "removed tools", Tools: []string{"write_file"}}
	if got := lines(); !reflect.DeepEqual(got, []line{removed, removed}) {
		t.Errorf("lines on refresh = %+v, want one removed line per endpoint", got)
	}
}
