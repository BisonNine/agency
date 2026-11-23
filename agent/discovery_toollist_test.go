// ClawEh
// License: MIT

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/spawnllm"

	climcp "go.bison9.com/agency/mcp"
	"go.bison9.com/agency/tools"
)

// The upstream MCP tool lists an agent offers its model, with discovery off,
// on, and on with namespaces pinned, and what the tool schemas cost once a
// stored search result has referenced hidden tools. The fixtures under
// testdata were captured before discovery moved to the SDK's discovery
// package and must not change. A server whose name starts with "mcp_" is
// among them: its tools are pinned by their internal name only.
func TestDiscovery_MCPToolListsUnchanged(t *testing.T) {
	for _, v := range []struct {
		name   string
		active bool
		pins   []string
	}{
		{"off", false, nil},
		{"on", true, nil},
		{"on_pinned", true, []string{"alpha", "bob_fetch"}},
	} {
		t.Run(v.name, func(t *testing.T) {
			al := newTestAgentLoop(t).al
			agent := al.registry.Default()
			agent.Config.MCPTools = []string{"alpha", "mcp_alpha", "bob"}
			agent.DiscoveryActive = v.active
			agent.AlwaysShownNamespaces = v.pins
			al.GetConfig().Tools.Discovery.Enabled = v.active
			al.GetConfig().Tools.Discovery.AlwaysShownNamespaces = v.pins
			for _, srv := range []struct {
				name  string
				tools []string
			}{
				{"alpha", []string{"read", "write"}},
				{"mcp_alpha", []string{"scan"}},
				{"bob", []string{"fetch", "post"}},
			} {
				var list []mcpgo.Tool
				for _, n := range srv.tools {
					list = append(list, mcpgo.NewTool(n, mcpgo.WithDescription("The "+n+" tool of "+srv.name+".\nSecond line.")))
				}
				conn := &climcp.ServerConnection{Name: srv.name, Tools: list}
				agent.Tools.ReplaceByPrefix(tools.MCPServerPrefix(srv.name), mcpServerToolEntries(agent, nil, srv.name, conn))
			}
			if v.active {
				al.registerDiscoveryMetaTools(t.Context(), agent, al.GetConfig())
			}

			sent := sentToolList(agent)
			history := []spawnllm.Message{
				{Role: "tool", ToolCallID: "tc-1", ToolReferences: []string{"bob_post", "alpha_read"}},
				{Role: "tool", ToolCallID: "tc-2", ToolReferences: []string{"alpha_read", "mcp_alpha_scan"}},
			}
			inDefs := inContextToolList(agent, history)
			inContext := make([]string, 0, len(inDefs))
			for _, d := range inDefs {
				inContext = append(inContext, d.Function.Name)
			}
			got, err := json.MarshalIndent(map[string]any{"sent": sent, "deferred": deferredNames(sent), "in_context": inContext}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "discovery_mcp_"+v.name+".json")
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
				t.Errorf("MCP tool list for %q differs from %s:\n got %s", v.name, path, got)
			}
		})
	}
}

// deferredNames lists the deferred and tool-search flags, which the JSON form
// of a definition leaves out.
func deferredNames(defs []spawnllm.ToolDefinition) []string {
	var out []string
	for _, d := range defs {
		if d.Deferred {
			out = append(out, "deferred:"+d.Function.Name)
		}
		if d.ToolSearch {
			out = append(out, "tool_search:"+d.Function.Name)
		}
	}
	return out
}

// sentToolList is the list a turn hands the provider.
func sentToolList(agent *AgentInstance) []spawnllm.ToolDefinition {
	return loop.ToolDefs(agent.Tools.Snapshot(), nil).Send
}

// inContextToolList is what the tool-schema estimate counts for history:
// discovery's InContext for what history's tool results have revealed, as
// the SDK agent computes it over the same registry snapshot.
func inContextToolList(agent *AgentInstance, history []spawnllm.Message) []spawnllm.ToolDefinition {
	var refs [][]string
	for _, m := range history {
		if len(m.ToolReferences) > 0 {
			refs = append(refs, m.ToolReferences)
		}
	}
	return loop.ToolDefs(agent.Tools.Snapshot(), discovery.NewSession(refs...)).InContext
}
