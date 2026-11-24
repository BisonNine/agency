// Agency
// License: MIT

package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Each shared knowledge base that cannot be used is refused on save with one
// sentence naming the agent and the entry; nothing is written.
func TestStoreUpdate_SharedKnowledgeRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shared []SharedKnowledgeConfig
		want   string
	}{
		{
			"no name",
			[]SharedKnowledgeConfig{{Path: "/srv/kb"}},
			`Main's shared knowledge base for "/srv/kb" needs a name.`,
		},
		{
			"slash",
			[]SharedKnowledgeConfig{{Name: "a/b", Path: "/srv/kb"}},
			`Main's shared knowledge base name "a/b" may contain only letters, digits, -, _ and . and may not start with a dot.`,
		},
		{
			"dot first",
			[]SharedKnowledgeConfig{{Name: ".hidden", Path: "/srv/kb"}},
			`Main's shared knowledge base name ".hidden" may contain only letters, digits, -, _ and . and may not start with a dot.`,
		},
		{
			"space",
			[]SharedKnowledgeConfig{{Name: "my kb", Path: "/srv/kb"}},
			`Main's shared knowledge base name "my kb" may contain only letters, digits, -, _ and . and may not start with a dot.`,
		},
		{
			"twice",
			[]SharedKnowledgeConfig{{Name: "team", Path: "/srv/a"}, {Name: "team", Path: "/srv/b"}},
			`Main has more than one shared knowledge base named "team".`,
		},
		{
			"relative",
			[]SharedKnowledgeConfig{{Name: "team", Path: "kb/team"}},
			`Main's shared knowledge base "team" must have an absolute folder path.`,
		},
		{
			"no path",
			[]SharedKnowledgeConfig{{Name: "team"}},
			`Main's shared knowledge base "team" must have an absolute folder path.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			err := s.Update(func(c *Config) error {
				c.Agents.List[0].Knowledge = &KnowledgeConfig{Enabled: true, Shared: tc.shared}
				return nil
			})
			if _, ok := errors.AsType[*ValidationError](err); !ok {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q, want %q", err.Error(), tc.want)
			}
			if s.Current().Agents.List[0].Knowledge != nil {
				t.Error("refused entry became current")
			}
		})
	}
}

// A valid block is saved as given; the same name may be used by two agents.
func TestStoreUpdate_SharedKnowledgeSaved(t *testing.T) {
	s := newTestStore(t)
	shared := []SharedKnowledgeConfig{{Name: "Pen-test_2.0", Path: "/srv/kb", Write: true}}
	err := s.Update(func(c *Config) error {
		c.Agents.List[0].Knowledge = &KnowledgeConfig{Enabled: true, Shared: shared}
		c.Agents.List = append(c.Agents.List, AgentConfig{ID: "bob", Knowledge: &KnowledgeConfig{Shared: shared}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Current().Agents.List[0].Knowledge
	if got == nil || !got.Enabled || !slices.Equal(got.Shared, shared) {
		t.Errorf("saved = %+v", got)
	}
}

// LoadConfig refuses a file with a bad shared knowledge base, naming it.
func TestLoadConfig_SharedKnowledgeRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"agents":{"list":[{"id":"alice","knowledge":{"enabled":true,"shared":[{"name":"team","path":"team"}]}}]}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `alice's shared knowledge base "team" must have an absolute folder path.`) {
		t.Fatalf("err = %v", err)
	}
}

func TestKnowledgeSwitch(t *testing.T) {
	c := &Config{Agents: AgentsConfig{List: []AgentConfig{
		{ID: "Alice", Knowledge: &KnowledgeConfig{Enabled: true}},
		{ID: "bob"},
		{ID: "carol", Knowledge: &KnowledgeConfig{Shared: []SharedKnowledgeConfig{{Name: "x", Path: "/x"}}}},
	}}}
	for id, want := range map[string]bool{"alice": true, "bob": false, "carol": false, "nobody": false} {
		if got := c.AgentSuiteEnabled(id, KnowledgeSuite); got != want {
			t.Errorf("AgentSuiteEnabled(%q) = %v, want %v", id, got, want)
		}
	}
	var none *AgentConfig
	if none.KnowledgeEnabled() || none.SharedKnowledge() != nil {
		t.Error("a nil agent has knowledge bases")
	}
	if got := c.Agents.List[2].SharedKnowledge(); len(got) != 1 {
		t.Errorf("SharedKnowledge = %v", got)
	}
}

func TestValidSharedKnowledgeName(t *testing.T) {
	for name, want := range map[string]bool{
		"pentesting": true, "Pen-test_2.0": true, "a.b": true, "9": true,
		"": false, ".x": false, "a/b": false, `a\b`: false, "a b": false, "é": false, "..": false,
	} {
		if got := ValidSharedKnowledgeName(name); got != want {
			t.Errorf("ValidSharedKnowledgeName(%q) = %v, want %v", name, got, want)
		}
	}
}

// MissingSharedKnowledgeFolders lists each entry with an absolute path that
// is not an existing directory, whether the switch is on or not.
func TestMissingSharedKnowledgeFolders(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	c := &Config{Agents: AgentsConfig{List: []AgentConfig{{ID: "alice", Knowledge: &KnowledgeConfig{Shared: []SharedKnowledgeConfig{
		{Name: "here", Path: dir}, {Name: "file", Path: file}, {Name: "gone", Path: missing}, {Name: "rel", Path: "rel"},
	}}}}}}
	want := []MissingSharedKnowledge{{Agent: "alice", Name: "file", Path: file}, {Agent: "alice", Name: "gone", Path: missing}}
	if got := c.MissingSharedKnowledgeFolders(); !slices.Equal(got, want) {
		t.Errorf("missing = %+v, want %+v", got, want)
	}
}
