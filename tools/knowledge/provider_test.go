// Agency
// License: MIT

package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// withStore installs a fresh store for the test.
func withStore(t *testing.T) {
	t.Helper()
	st, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	SetStore(st)
	t.Cleanup(func() {
		SetStore(nil)
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
}

// testConfig has alice (knowledge on, one read-only shared bundle) and bob
// (off), each with a workspace of its own.
func testConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	shared := filepath.Join(dir, "ref")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Agents.List = []config.AgentConfig{
		{ID: "Alice", Workspace: filepath.Join(dir, "alice"), Knowledge: &config.KnowledgeConfig{
			Enabled: true, Shared: []config.SharedKnowledgeConfig{{Name: "ref", Path: shared}},
		}},
		{ID: "bob", Workspace: filepath.Join(dir, "bob"), Knowledge: &config.KnowledgeConfig{
			Shared: []config.SharedKnowledgeConfig{{Name: "ref", Path: shared}},
		}},
	}
	return cfg, dir
}

func build(cfg *config.Config, agentID, workspace string) []toolkit.Tool {
	p := tools.NamespacedProvider(Namespace, GlobalProvider)
	return p.Build(tools.ToolDeps{Cfg: cfg, AgentID: agentID, Workspace: workspace})
}

func names(ts []toolkit.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	slices.Sort(out)
	return out
}

func TestMetadata(t *testing.T) {
	if GlobalProvider.Namespace() != "kb" || GlobalProvider.Suite() != "knowledge" ||
		GlobalProvider.Description() != "Knowledge bases: read, search and write OKF bundles" {
		t.Errorf("metadata: %q %q %q", GlobalProvider.Namespace(), GlobalProvider.Suite(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	// The catalogue shows the suite as one entry, managed by its switch.
	d := tools.NamespacedProvider(Namespace, GlobalProvider).Describe()
	if len(d) != 1 || d[0].Name != "kb" || d[0].Suite != "knowledge" {
		t.Errorf("Describe = %+v", d)
	}
}

// The three tools exist only for an agent whose switch is on, in a process
// with a store and for an agent with a workspace.
func TestRegisterTools_Gating(t *testing.T) {
	cfg, dir := testConfig(t)
	alice := filepath.Join(dir, "alice")
	if got := build(cfg, "alice", alice); len(got) != 0 {
		t.Errorf("no store: %v", names(got))
	}
	withStore(t)
	want := []string{"kb_get", "kb_put", "kb_search"}
	if got := names(build(cfg, "alice", alice)); !slices.Equal(got, want) {
		t.Errorf("alice = %v, want %v", got, want)
	}
	if got := build(cfg, "bob", filepath.Join(dir, "bob")); len(got) != 0 {
		t.Errorf("bob (off) = %v", names(got))
	}
	if got := build(cfg, "alice", ""); len(got) != 0 {
		t.Errorf("no workspace = %v", names(got))
	}
	// Asked directly for an agent without the switch, the provider builds
	// nothing either.
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: cfg, AgentID: "bob"}); len(defs) != 0 {
		t.Errorf("bob asked directly = %d tools", len(defs))
	}
	// Enumeration (no config) builds nothing.
	if defs := GlobalProvider.RegisterTools(toolspec.Deps{AgentID: "alice"}); len(defs) != 0 {
		t.Errorf("enumeration = %d tools", len(defs))
	}
}

func call(t *testing.T, tool toolkit.Tool, args map[string]any) map[string]any {
	t.Helper()
	res := tool.Execute(context.Background(), args)
	var out map[string]any
	if err := json.Unmarshal([]byte(res.ForLLM), &out); err != nil {
		t.Fatalf("%s %v: result %q is not JSON: %v", tool.Name(), args, res.ForLLM, err)
	}
	if res.IsError {
		out["is_error"] = true
	}
	return out
}

func toolNamed(ts []toolkit.Tool, name string) toolkit.Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// The tools act on the agent's own resolver: an own bundle is created in its
// workspace, a shared one is read-only, an unknown name is refused naming it.
// A clone (built with its source's id and workspace) reaches the same
// bundles.
func TestTools_OwnAndShared(t *testing.T) {
	withStore(t)
	cfg, dir := testConfig(t)
	ws := filepath.Join(dir, "alice")
	ts := build(cfg, "alice", ws)

	put := call(t, toolNamed(ts, "kb_put"), map[string]any{
		"kb": "notes", "concept_id": "guide/crypto", "frontmatter": map[string]any{"type": "Note", "title": "Crypto"},
		"body": "Record each key size.\n",
	})
	if put["status"] != "created" {
		t.Fatalf("put = %v", put)
	}
	if _, err := os.Stat(filepath.Join(ws, "okf", "notes", "guide", "crypto.md")); err != nil {
		t.Errorf("concept file: %v", err)
	}
	ro := call(t, toolNamed(ts, "kb_put"), map[string]any{
		"kb": "shared/ref", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": "",
	})
	if ro["is_error"] != true || !strings.Contains(fmt.Sprint(ro["error"]), `"shared/ref"`) {
		t.Errorf("put to read-only = %v", ro)
	}
	unknown := call(t, toolNamed(ts, "kb_get"), map[string]any{"kb": "shared/nope", "concept_id": "a"})
	if unknown["is_error"] != true || !strings.Contains(fmt.Sprint(unknown["error"]), `"shared/nope"`) {
		t.Errorf("get from unknown = %v", unknown)
	}

	clone := build(cfg, "alice", ws) // a clone's deps: its source's id and workspace
	got := call(t, toolNamed(clone, "kb_get"), map[string]any{"kb": "notes", "concept_id": "guide/crypto"})
	if got["body"] != "Record each key size.\n" {
		t.Errorf("clone get = %v", got)
	}
}

// Store is the installed store, nil without one.
func TestStore(t *testing.T) {
	if Store() != nil {
		t.Fatal("a store before SetStore")
	}
	withStore(t)
	if Store() == nil {
		t.Error("no store after SetStore")
	}
}
