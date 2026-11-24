// Agency
// License: MIT

// Package toollist pins the tool list each agent offers its model, with
// progressive discovery off and on, for the production-shaped configuration
// of tests/capabilities. The fixtures under testdata were captured before
// discovery moved to the SDK's discovery package; the list must stay byte
// for byte what it was (names, descriptions, schemas, order and the
// deferred and tool-search flags).
package toollist

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/gateway"
	"go.bison9.com/agency/tools/knowledge"
)

// variant is one configuration the lists are captured for. knowledge runs
// it with a knowledge-base store, as the gateway does.
type variant struct {
	name      string
	apply     func(cfg *config.Config)
	knowledge bool
}

var variants = []variant{
	{name: "off", apply: func(cfg *config.Config) { cfg.Tools.Discovery.Enabled = false }},
	{name: "on", apply: func(cfg *config.Config) { cfg.Tools.Discovery.Enabled = true }},
	{name: "on_pinned", apply: func(cfg *config.Config) {
		cfg.Tools.Discovery.Enabled = true
		cfg.Tools.Discovery.AlwaysShownNamespaces = []string{"wxca", "file"}
		cfg.Tools.Discovery.MaxSearchResults = 4
		for i := range cfg.Agents.List {
			a := &cfg.Agents.List[i]
			switch a.ID {
			case "alice":
				a.Maestro = &config.MaestroConfig{Enabled: true}
			case "bob":
				a.DenyTools = append(a.DenyTools, "get_tool_details")
			}
		}
	}},
	{name: "off_maestro", apply: func(cfg *config.Config) {
		cfg.Tools.Discovery.Enabled = false
		cfg.Tools.Discovery.AlwaysShownNamespaces = []string{"wxca"}
		for i := range cfg.Agents.List {
			if cfg.Agents.List[i].ID == "alice" {
				cfg.Agents.List[i].Maestro = &config.MaestroConfig{Enabled: true}
			}
		}
	}},
	// Maestro and knowledge bases both on: Maestro's tasks take knowledge.
	{name: "off_maestro_knowledge", knowledge: true, apply: func(cfg *config.Config) {
		cfg.Tools.Discovery.Enabled = false
		cfg.Tools.Discovery.AlwaysShownNamespaces = []string{"wxca"}
		for i := range cfg.Agents.List {
			if cfg.Agents.List[i].ID == "alice" {
				cfg.Agents.List[i].Maestro = &config.MaestroConfig{Enabled: true}
				cfg.Agents.List[i].Knowledge = &config.KnowledgeConfig{Enabled: true}
			}
		}
	}},
}

// def is one tool definition as the model receives it.
type def struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Deferred    bool           `json:"deferred,omitempty"`
	ToolSearch  bool           `json:"tool_search,omitempty"`
}

func TestToolListsUnchanged(t *testing.T) {
	gateway.RegisterToolProvidersForTest()
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			got := capture(t, v)
			path := filepath.Join("testdata", v.name+".json")
			if os.Getenv("UPDATE_TOOLLIST") == "1" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if string(want) != string(got) {
				t.Errorf("tool lists for %q differ from the fixture %s", v.name, path)
				diffAgents(t, want, got)
			}
		})
	}
}

// capture builds the loop for v and returns every agent's tool list as
// indented JSON, keyed by agent id.
func capture(t *testing.T, v variant) []byte {
	t.Helper()
	home := t.TempDir()
	copyFile(t, "../capabilities/testdata/fixture.json", filepath.Join(home, "config.json"))
	copyTree(t, "../capabilities/testdata/fusion", filepath.Join(home, "fusion"))
	t.Setenv("CLAW_HOME", home)
	cfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	v.apply(cfg)
	if v.knowledge {
		store, kbErr := kb.New(kb.Options{})
		if kbErr != nil {
			t.Fatalf("kb.New: %v", kbErr)
		}
		knowledge.SetStore(store)
		t.Cleanup(func() {
			knowledge.SetStore(nil)
			if closeErr := store.Close(); closeErr != nil {
				t.Errorf("close the store: %v", closeErr)
			}
		})
	}

	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	t.Cleanup(al.GetRegistry().Close)

	ids := al.GetRegistry().All()
	sort.Strings(ids)
	out := map[string][]def{}
	for _, id := range ids {
		inst, ok := al.GetRegistry().Get(id)
		if !ok {
			t.Fatalf("agent %s not found", id)
		}
		out[id] = toDefs(sentToolList(inst))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// sentToolList is the list the agent's turn hands the provider.
func sentToolList(inst *agent.AgentInstance) []spawnllm.ToolDefinition {
	return loop.ToolDefs(inst.Tools.Snapshot(), nil).Send
}

func toDefs(in []spawnllm.ToolDefinition) []def {
	out := make([]def, 0, len(in))
	for _, d := range in {
		out = append(out, def{
			Type: d.Type, Name: d.Function.Name, Description: d.Function.Description,
			Parameters: d.Function.Parameters, Deferred: d.Deferred, ToolSearch: d.ToolSearch,
		})
	}
	return out
}

// diffAgents reports, per agent, the first definition that differs.
func diffAgents(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g map[string][]json.RawMessage
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	for id, wl := range w {
		gl := g[id]
		if len(wl) != len(gl) {
			t.Errorf("agent %s: %d tools, want %d", id, len(gl), len(wl))
		}
		for i := 0; i < len(wl) && i < len(gl); i++ {
			if string(wl[i]) != string(gl[i]) {
				t.Errorf("agent %s, tool %d:\n got %s\nwant %s", id, i, gl[i], wl[i])
				break
			}
		}
	}
	for id := range g {
		if _, ok := w[id]; !ok {
			t.Errorf("agent %s is not in the fixture", id)
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		copyFile(t, path, target)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
