// Agency
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/kbhost"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
	toolsknowledge "go.bison9.com/agency/tools/knowledge"
)

var kbToolNames = []string{"kb_get", "kb_put", "kb_search"}

const kbNotice = "\n\nNote: Bundle names may conflict. Do not assume that \"mybundle\" and \"shared/mybundle\" are the same."

// knowledgeConfig has main (knowledge bases on, the file tools granted) and
// bob (off), discovery off so every tool is listed.
func knowledgeConfig(t *testing.T, shared ...config.SharedKnowledgeConfig) *config.Config {
	t.Helper()
	cfg := ownerTestConfig(t)
	cfg.Tools.Discovery.Enabled = false
	cfg.Agents.List = []config.AgentConfig{
		{
			ID: "main", Name: "Main", Default: true, Tools: []string{"file_read_lines", "file_write"},
			Knowledge: &config.KnowledgeConfig{Enabled: true, Shared: shared},
		},
		{ID: "bob", Name: "Bob", Knowledge: &config.KnowledgeConfig{Shared: shared}},
	}
	return cfg
}

// newKnowledgeLoop is a loop over cfg in a process with a knowledge-base
// store and the kb provider, as the gateway sets them up.
func newKnowledgeLoop(t *testing.T, cfg *config.Config) *AgentLoop {
	t.Helper()
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tools.RegisterProvider(tools.NamespacedProvider(toolsknowledge.Namespace, toolsknowledge.GlobalProvider))
	st, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	toolsknowledge.SetStore(st)
	t.Cleanup(func() {
		toolsknowledge.SetStore(nil)
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
}

func configured(t *testing.T, al *AgentLoop, id string) *AgentInstance {
	t.Helper()
	inst, ok := al.GetRegistry().GetConfigured(id)
	if !ok {
		t.Fatalf("no agent %s", id)
	}
	return inst
}

// kbTools are the kb_* tools inst has.
func kbTools(inst *AgentInstance) []string {
	var out []string
	for _, n := range inst.Tools.List() {
		if strings.HasPrefix(n, "kb_") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// The switch grants the three tools; an agent without it does not have them
// registered or callable, and deny_tools still removes one.
func TestKnowledge_ToolsOnlyWithSwitch(t *testing.T) {
	cfg := knowledgeConfig(t)
	cfg.Agents.List[0].DenyTools = []string{"kb_put"}
	al := newKnowledgeLoop(t, cfg)

	if got := kbTools(configured(t, al, "main")); !slices.Equal(got, []string{"kb_get", "kb_search"}) {
		t.Errorf("main (kb_put denied) = %v", got)
	}
	bob := configured(t, al, "bob")
	if got := kbTools(bob); len(got) != 0 {
		t.Errorf("bob (off) = %v", got)
	}
	res := bob.Tools.ExecuteWithContext(context.Background(), "kb_get", map[string]any{"kb": "shared/x", "concept_id": "a"}, "cli", "direct", nil)
	if !res.IsError {
		t.Errorf("bob called kb_get: %q", res.ForLLM)
	}
}

// One turn: the model puts a concept into a new own bundle, gets it back
// and finds it by searching.
func TestKnowledge_TurnPutGetSearch(t *testing.T) {
	al := newKnowledgeLoop(t, knowledgeConfig(t))
	main := configured(t, al, "main")
	if got := kbTools(main); !slices.Equal(got, kbToolNames) {
		t.Fatalf("main = %v", got)
	}
	call := func(id, name string, args map[string]any) spawnllm.ToolCall {
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return spawnllm.ToolCall{
			ID: id, Type: "function", Name: name,
			Function: &spawnllm.FunctionCall{Name: name, Arguments: string(data)},
		}
	}
	provider := &sequenceProvider{
		responses: []*spawnllm.LLMResponse{
			{ToolCalls: []spawnllm.ToolCall{call("tc-1", "kb_put", map[string]any{
				"kb": "audit-methods", "concept_id": "guidance/crypto-review",
				"frontmatter": map[string]any{"type": "Evaluation Guidance", "title": "Review cryptographic claims"},
				"body":        "Record the evidence for each claimed algorithm and key size.\n",
			})}},
			{ToolCalls: []spawnllm.ToolCall{call("tc-2", "kb_get", map[string]any{
				"kb": "audit-methods", "concept_id": "guidance/crypto-review",
			})}},
			{ToolCalls: []spawnllm.ToolCall{call("tc-3", "kb_search", map[string]any{
				"kb": "audit-methods", "query": "key size",
			})}},
			{Content: "done"},
		},
		errors: []error{nil, nil, nil, nil},
	}
	setTestProvider(main, provider)
	opts := processOptions{SessionKey: "kb-turn", Channel: "cli", ChatID: "direct"}
	cm, release := al.getContextManager(main, opts.SessionKey)
	defer release()
	if _, err := al.runLLMIteration(context.Background(), main, []spawnllm.Message{{Role: "user", Content: "go"}}, opts, cm, nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	results := map[string]string{}
	for _, m := range provider.lastMessages {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(results["tc-1"], `"status":"created"`) {
		t.Errorf("put = %q", results["tc-1"])
	}
	if !strings.Contains(results["tc-2"], "Record the evidence for each claimed algorithm and key size.") {
		t.Errorf("get = %q", results["tc-2"])
	}
	if !strings.Contains(results["tc-3"], `"concept_id":"guidance/crypto-review"`) {
		t.Errorf("search = %q", results["tc-3"])
	}
	if _, err := os.Stat(filepath.Join(kbhost.OwnDir(main.Workspace), "audit-methods", "guidance", "crypto-review.md")); err != nil {
		t.Errorf("concept file: %v", err)
	}
}

// The prompt lists the bundles exactly, own first then shared, read-only
// ones marked, and follows a new own bundle; an agent without the switch
// gets nothing.
func TestKnowledge_Prompt(t *testing.T) {
	sharedDir := t.TempDir()
	al := newKnowledgeLoop(t, knowledgeConfig(t,
		config.SharedKnowledgeConfig{Name: "ref", Path: sharedDir},
		config.SharedKnowledgeConfig{Name: "gone", Path: filepath.Join(sharedDir, "missing"), Write: true},
	))
	main := configured(t, al, "main")
	prompt := main.ContextBuilder.BuildSystemPromptWithCache()
	want := "\n\nBundles: shared/ref (read only)" + kbNotice + "\n\n## Important Rules\n\n"
	if !strings.Contains(prompt, want) {
		t.Fatalf("prompt lacks %q:\n%s", want, prompt)
	}
	if err := os.MkdirAll(filepath.Join(kbhost.OwnDir(main.Workspace), "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	prompt = main.ContextBuilder.BuildSystemPromptWithCache()
	if want := "\n\nBundles: notes, shared/ref (read only)" + kbNotice + "\n\n## Important Rules"; !strings.Contains(prompt, want) {
		t.Errorf("after a new own bundle the prompt lacks %q:\n%s", want, prompt)
	}
	if got := configured(t, al, "bob").ContextBuilder.BuildSystemPromptWithCache(); strings.Contains(got, "Bundles:") {
		t.Errorf("bob (off) has bundles in its prompt:\n%s", got)
	}
}

func TestKnowledge_PromptNoneYet(t *testing.T) {
	al := newKnowledgeLoop(t, knowledgeConfig(t))
	prompt := configured(t, al, "main").ContextBuilder.BuildSystemPromptWithCache()
	if want := "\n\nBundles: none yet" + kbNotice + "\n\n## Important Rules"; !strings.Contains(prompt, want) {
		t.Errorf("prompt lacks %q:\n%s", want, prompt)
	}
}

// A clone has its source's knowledge bases: the tools, the bundles they
// reach (its puts land in the source's workspace) and the prompt lines.
func TestKnowledge_Clone(t *testing.T) {
	al := newKnowledgeLoop(t, knowledgeConfig(t))
	main := configured(t, al, "main")
	cloneID, err := al.GetRegistry().CreateClone("main")
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := al.GetRegistry().Get(cloneID)
	if got := kbTools(clone); !slices.Equal(got, kbToolNames) {
		t.Fatalf("clone tools = %v", got)
	}
	res := clone.Tools.ExecuteWithContext(context.Background(), "kb_put", map[string]any{
		"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": "x\n",
	}, "cli", "direct", nil)
	if res.IsError {
		t.Fatalf("clone put: %s", res.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(kbhost.OwnDir(main.Workspace), "notes", "a.md")); err != nil {
		t.Errorf("the clone's put is not in its source's bundle: %v", err)
	}
	if got := clone.ContextBuilder.BuildSystemPromptWithCache(); !strings.Contains(got, "Bundles: notes"+kbNotice) {
		t.Errorf("clone prompt:\n%s", got)
	}
}

// The file tools cannot read or write okf/: knowledge bases are reached
// only through the kb_* tools.
func TestKnowledge_FileToolsRefuseOwnDir(t *testing.T) {
	al := newKnowledgeLoop(t, knowledgeConfig(t))
	main := configured(t, al, "main")
	put := main.Tools.ExecuteWithContext(context.Background(), "kb_put", map[string]any{
		"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": "secret\n",
	}, "cli", "direct", nil)
	if put.IsError {
		t.Fatalf("put: %s", put.ForLLM)
	}
	for _, name := range []string{"file_read_lines", "file_write"} {
		if _, ok := main.Tools.Get(name); !ok {
			t.Fatalf("main has no %s", name)
		}
	}
	for _, p := range []string{"okf/notes/a.md", filepath.Join(kbhost.OwnDir(main.Workspace), "notes", "a.md")} {
		read := main.Tools.ExecuteWithContext(context.Background(), "file_read_lines", map[string]any{"path": p}, "cli", "direct", nil)
		if !read.IsError || !strings.Contains(read.ForLLM, "read denied") || strings.Contains(read.ForLLM, "secret") {
			t.Errorf("file_read_lines %s = %v %q", p, read.IsError, read.ForLLM)
		}
		write := main.Tools.ExecuteWithContext(context.Background(), "file_write", map[string]any{"path": p, "content": "x"}, "cli", "direct", nil)
		if !write.IsError {
			t.Errorf("file_write %s succeeded", p)
		}
	}
	if got, err := os.ReadFile(filepath.Join(kbhost.OwnDir(main.Workspace), "notes", "a.md")); err != nil || !strings.Contains(string(got), "secret") {
		t.Errorf("concept changed: %q %v", got, err)
	}
}

// A reload applies the switch and the shared list: tools and prompt follow
// the new configuration.
func TestKnowledge_Reload(t *testing.T) {
	cfg := knowledgeConfig(t)
	al := newKnowledgeLoop(t, cfg)
	ctx := context.Background()
	reload := func(change func(c *config.Config)) {
		t.Helper()
		next, err := al.GetConfig().Clone()
		if err != nil {
			t.Fatal(err)
		}
		change(next)
		if err := reloadForTest(t, al, ctx, &mockProvider{}, next); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}

	reload(func(c *config.Config) { c.Agents.List[0].Knowledge.Enabled = false })
	main := configured(t, al, "main")
	if got := kbTools(main); len(got) != 0 {
		t.Errorf("switched off: tools %v", got)
	}
	if strings.Contains(main.ContextBuilder.BuildSystemPromptWithCache(), "Bundles:") {
		t.Error("switched off: bundles still in the prompt")
	}

	shared := t.TempDir()
	reload(func(c *config.Config) {
		c.Agents.List[0].Knowledge.Enabled = true
		c.Agents.List[0].Knowledge.Shared = []config.SharedKnowledgeConfig{{Name: "team", Path: shared, Write: true}}
	})
	main = configured(t, al, "main")
	if got := kbTools(main); !slices.Equal(got, kbToolNames) {
		t.Errorf("switched on: tools %v", got)
	}
	if got := main.ContextBuilder.BuildSystemPromptWithCache(); !strings.Contains(got, "Bundles: shared/team"+kbNotice) {
		t.Errorf("prompt after adding a shared bundle:\n%s", got)
	}
	res := main.Tools.ExecuteWithContext(ctx, "kb_put", map[string]any{
		"kb": "shared/team", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": "",
	}, "cli", "direct", nil)
	if res.IsError {
		t.Errorf("put to the new shared bundle: %s", res.ForLLM)
	}

	reload(func(c *config.Config) { c.Agents.List[0].Knowledge.Shared = nil })
	res = configured(t, al, "main").Tools.ExecuteWithContext(ctx, "kb_get", map[string]any{"kb": "shared/team", "concept_id": "a"}, "cli", "direct", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "unknown") {
		t.Errorf("get from a removed shared bundle = %v %q", res.IsError, res.ForLLM)
	}
}

// The prompt lists knowledge bases only while the agent has a knowledge-base
// tool in this process: not without a store (as in the CLI), not with all
// three denied; denying only kb_put keeps the lines (it can still read).
func TestKnowledge_PromptFollowsTheTools(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
		tools.RegisterProvider(tools.NamespacedProvider(toolsknowledge.Namespace, toolsknowledge.GlobalProvider))
		al := mustNewAgentLoop(t, knowledgeConfig(t), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
		main := configured(t, al, "main")
		if got := kbTools(main); len(got) != 0 {
			t.Fatalf("tools without a store = %v", got)
		}
		if got := main.ContextBuilder.BuildSystemPromptWithCache(); strings.Contains(got, "Bundles:") {
			t.Errorf("bundles listed without the tools:\n%s", got)
		}
	})
	for _, tc := range []struct {
		name   string
		deny   []string
		listed bool
	}{
		{"all denied", []string{"kb_get", "kb_search", "kb_put"}, false},
		{"all denied by prefix", []string{"kb_*"}, false},
		{"only kb_put denied", []string{"kb_put"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := knowledgeConfig(t)
			cfg.Agents.List[0].DenyTools = tc.deny
			main := configured(t, newKnowledgeLoop(t, cfg), "main")
			got := main.ContextBuilder.BuildSystemPromptWithCache()
			if listed := strings.Contains(got, "Bundles: none yet"+kbNotice); listed != tc.listed {
				t.Errorf("bundles listed = %v, want %v:\n%s", listed, tc.listed, got)
			}
		})
	}
}

// The cached prompt follows the tools: a prompt built before the tools were
// registered gains the lines once they are.
func TestKnowledge_PromptCacheFollowsTheTools(t *testing.T) {
	has := false
	cb := NewContextBuilder(t.TempDir()).
		WithKnowledge(kbhost.New(nil, nil, t.TempDir())).
		WithKnowledgeTools(func() bool { return has })
	if strings.Contains(cb.BuildSystemPromptWithCache(), "Bundles:") {
		t.Fatal("bundles listed without the tools")
	}
	has = true
	if !strings.Contains(cb.BuildSystemPromptWithCache(), "Bundles: none yet") {
		t.Error("the cached prompt did not gain the bundles")
	}
}
