// ClawEh
// License: MIT

package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A config.json that still has memory.consolidation.propose_domains and
// auto_promote, in the defaults and on an agent, loads without error or
// unknown-key warning, and the next save drops them.
func TestMemoryConsolidation_RetiredKeys(t *testing.T) {
	s := newTestStore(t)
	cfg, err := LoadConfig(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Defaults.Memory.Consolidation.IdleMinutes = 7
	mem := cfg.Agents.Defaults.Memory
	cfg.Agents.List[0].Memory = &mem
	if err = writeRawConfig(s.Path(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	const anchor = `"debug_dump": false`
	if n := strings.Count(string(data), anchor); n != 2 {
		t.Fatalf("%d consolidation blocks in the file, want 2", n)
	}
	data = []byte(strings.ReplaceAll(string(data), anchor, anchor+`, "propose_domains": true, "auto_promote": true`))
	if err = os.WriteFile(s.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	decoded, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := unknownConfigKeys(decoded); len(got) != 0 {
		t.Errorf("unknownConfigKeys = %v, want none", got)
	}
	loaded, err := LoadConfig(s.Path())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Agents.Defaults.Memory.Consolidation.IdleMinutes != 7 || loaded.Agents.List[0].Memory == nil ||
		loaded.Agents.List[0].Memory.Consolidation.IdleMinutes != 7 {
		t.Fatal("the rest of the consolidation block was not loaded")
	}

	if _, err = s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err = s.Update(func(c *Config) error {
		c.Agents.List[0].Name = "Alice"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	saved, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"propose_domains", "auto_promote"} {
		if strings.Contains(string(saved), key) {
			t.Errorf("a save kept %s", key)
		}
	}
	if !strings.Contains(string(saved), `"idle_minutes": 7`) {
		t.Error("a save lost idle_minutes")
	}
}

// A WebUI patch carrying the removed consolidation keys is not refused for
// them; a typo next to them still is.
func TestUnknownPatchKeys_RetiredConsolidationKeys(t *testing.T) {
	patch := map[string]any{"agents": map[string]any{
		"defaults": map[string]any{"memory": map[string]any{"consolidation": map[string]any{
			"propose_domains": true, "auto_promote": true, "idle_minutes": 7,
		}}},
		"list": []any{map[string]any{"id": "bob", "memory": map[string]any{"consolidation": map[string]any{
			"propose_domains": false, "auto_promote": false, "nightly_att": "x",
		}}}},
	}}
	want := "agents.list[0].memory.consolidation.nightly_att"
	if got := UnknownPatchKeys(patch); len(got) != 1 || got[0] != want {
		t.Errorf("UnknownPatchKeys = %v, want [%s]", got, want)
	}
}

// The retired type keeps no value and is never written.
func TestRetiredKey(t *testing.T) {
	var c MemoryConsolidationConfig
	if err := json.Unmarshal([]byte(`{"propose_domains": {"any": [1]}, "auto_promote": "yes"}`), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "propose_domains") || strings.Contains(string(out), "auto_promote") {
		t.Errorf("marshalled %s", out)
	}
}
