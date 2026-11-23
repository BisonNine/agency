// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/tools/knowledge"
)

// knowledgeRegistry is a registry holding what the knowledge provider
// builds for agentID under cfg, beside a plain tool every agent has.
func knowledgeRegistry(cfg *config.Config, agentID, workspace string) *tools.ToolRegistry {
	r := newRegistryWith(&mockTool{name: "read_file", params: map[string]any{}, result: toolkit.NewResult("ok")})
	for _, tl := range tools.NamespacedProvider(knowledge.Namespace, knowledge.GlobalProvider).Build(
		tools.ToolDeps{Cfg: cfg, AgentID: agentID, Workspace: workspace}) {
		r.RegisterSuite(tl)
	}
	return r
}

// The kb_* tools are published, listed and callable over MCP only for an
// agent whose knowledge switch is on.
func TestKnowledgeTools_OnlyForAgentsWithTheSwitch(t *testing.T) {
	st, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	knowledge.SetStore(st)
	t.Cleanup(func() {
		knowledge.SetStore(nil)
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Agents.List = []config.AgentConfig{
		{ID: "alice", Knowledge: &config.KnowledgeConfig{Enabled: true}},
		{ID: "bob"},
	}
	hasKB := func(listed map[string]listedTool) []string {
		var out []string
		for name := range listed {
			if strings.HasPrefix(name, "kb_") {
				out = append(out, name)
			}
		}
		return out
	}

	// Nobody has the switch: no kb tool anywhere.
	offCfg := &config.Config{}
	offCfg.Agents.List = []config.AgentConfig{{ID: "alice"}, {ID: "bob"}}
	offTokens, _ := seedSessionToken("alice")
	off := hostFor(t, map[string]*tools.ToolRegistry{
		"alice": knowledgeRegistry(offCfg, "alice", filepath.Join(dir, "alice")),
		"bob":   knowledgeRegistry(offCfg, "bob", filepath.Join(dir, "bob")),
	}, offTokens)
	if got := hasKB(listTools(t, off, InternalEndpointPath, "")); len(got) != 0 {
		t.Errorf("switch off everywhere, /internal lists %v", got)
	}

	tokens := newSessionTokenStore()
	aliceTok := tokens.Issue("alice", "agent:alice:main", "/ws")
	bobTok := tokens.Issue("bob", "agent:bob:main", "/ws")
	regs := map[string]*tools.ToolRegistry{
		"alice": knowledgeRegistry(cfg, "alice", filepath.Join(dir, "alice")),
		"bob":   knowledgeRegistry(cfg, "bob", filepath.Join(dir, "bob")),
	}
	srv := hostFor(t, regs, tokens)
	if got := hasKB(listTools(t, srv, DefaultEndpointPath, aliceTok)); len(got) != 3 {
		t.Errorf("alice on /mcp lists %v, want the three kb tools", got)
	}
	if got := hasKB(listTools(t, srv, DefaultEndpointPath, bobTok)); len(got) != 0 {
		t.Errorf("bob on /mcp lists %v", got)
	}

	resolve := resolverFor(regs)
	out, isErr := dispatchToolCall(context.Background(), "kb_get",
		map[string]any{"session_token": bobTok, "kb": "notes", "concept_id": "a"},
		tokens, resolve, nil, nil, nil, nil)
	if !isErr || out != toolkit.NotEnabledMessage("kb_get") {
		t.Errorf("bob kb_get = (%q, %v)", out, isErr)
	}
	out, isErr = dispatchToolCall(context.Background(), "kb_put",
		map[string]any{
			"session_token": aliceTok, "kb": "notes", "concept_id": "a",
			"frontmatter": map[string]any{"type": "Note"}, "body": "x\n",
		},
		tokens, resolve, nil, nil, nil, nil)
	if isErr || !strings.Contains(out, `"status":"created"`) {
		t.Errorf("alice kb_put = (%q, %v)", out, isErr)
	}
}
