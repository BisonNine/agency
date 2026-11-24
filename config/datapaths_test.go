// Agency
// License: MIT

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/global"
)

func TestToStoredPath(t *testing.T) {
	home := filepath.FromSlash("/srv/agency")
	cases := []struct {
		name, path, want string
	}{
		{"inside", "/srv/agency/agents", "agents"},
		{"nested", "/srv/agency/tls/my.crt", "tls/my.crt"},
		{"not clean", "/srv/agency/./agents/../agents/alice/", "agents/alice"},
		{"the data dir itself", "/srv/agency", "."},
		{"outside", "/srv/other/agents", "/srv/other/agents"},
		{"sibling with the same prefix", "/srv/agency-old/agents", "/srv/agency-old/agents"},
		{"parent", "/srv", "/srv"},
		{"relative kept", "agents/alice", "agents/alice"},
		{"tilde kept", "~/notes", "~/notes"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToStoredPath(home, filepath.FromSlash(tc.path)); got != filepath.FromSlash(tc.want) && got != tc.want {
				t.Errorf("ToStoredPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
	if got := ToStoredPath("", "/srv/agency/agents"); got != "/srv/agency/agents" {
		t.Errorf("without a data dir = %q, want the path as it is", got)
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	home := "/srv/agency"
	cases := []struct {
		name, path, want string
	}{
		{"relative", "agents", "/srv/agency/agents"},
		{"relative with slashes", "tls/my.crt", "/srv/agency/tls/my.crt"},
		{"absolute", "/srv/other/agents", "/srv/other/agents"},
		{"absolute inside", "/srv/agency/agents", "/srv/agency/agents"},
		{"tilde", "~/notes", "/home/alice/notes"},
		{"dot", ".", "/srv/agency"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePath(home, tc.path); got != tc.want {
				t.Errorf("ResolvePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
	// Every stored form resolves to the path it was made from.
	for _, p := range []string{"/srv/agency/agents/alice", "/srv/agency", "/srv/other"} {
		if got := ResolvePath(home, ToStoredPath(home, p)); got != p {
			t.Errorf("round trip of %q = %q", p, got)
		}
	}
}

// pathsConfig is a config.json with every path setting set: inside the data
// directory absolute, inside relative, and outside absolute.
func pathsConfig(home, outside string) string {
	doc := map[string]any{
		"gateway": map[string]any{"tls": map[string]any{
			"cert_file": filepath.Join(home, "tls", "own.crt"),
			"key_file":  "tls/own.key",
		}},
		"backup": map[string]any{"dest": filepath.Join(outside, "backups")},
		"models": []any{map[string]any{
			"model_name": "m", "model": "x", "provider": "p",
			"workspace":         filepath.Join(home, "cli-alice"),
			"response_log_file": "logs/responses.log",
		}},
		"tools": map[string]any{"mcp": map[string]any{"servers": map[string]any{
			"files": map[string]any{"command": "srv", "env_file": filepath.Join(home, "mcp.env")},
		}}},
		"agents": map[string]any{
			"base_dir":   filepath.Join(home, "agents"),
			"common_dir": "shared",
			"list": []any{
				map[string]any{"id": "alice", "workspace": filepath.Join(home, "agents", "alice")},
				map[string]any{"id": "bob", "workspace": filepath.Join(outside, "bob")},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A config.json holding absolute paths inside the data directory loads with
// the full paths, and the next save writes them relative; absolute paths
// outside stay absolute, and relative ones name a place in the data
// directory.
func TestPaths_LoadAbsoluteSaveRelative(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(pathsConfig(home, outside)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Current()
	full := map[string]string{
		"agents.base_dir":          cfg.Agents.BaseDir,
		"agents.common_dir":        cfg.Agents.CommonDir,
		"alice workspace":          cfg.Agents.List[0].Workspace,
		"bob workspace":            cfg.Agents.List[1].Workspace,
		"cert_file":                cfg.Gateway.TLS.CertFile,
		"key_file":                 cfg.Gateway.TLS.KeyFile,
		"backup.dest":              cfg.Backup.Dest,
		"model workspace":          cfg.Models[0].Workspace,
		"model response_log_file":  cfg.Models[0].ResponseLogFile,
		"mcp env_file":             cfg.Tools.MCP.Servers["files"].EnvFile,
		"BaseDir()":                cfg.BaseDir(),
		"ResolveCommonDir()":       cfg.ResolveCommonDir(),
		"BackupDir()":              cfg.BackupDir(),
		"AgentSessionDirs()[0]":    cfg.AgentSessionDirs()[0],
		"AgentDirs()[1] (alice's)": cfg.AgentDirs()[1],
	}
	want := map[string]string{
		"agents.base_dir":          filepath.Join(home, "agents"),
		"agents.common_dir":        filepath.Join(home, "shared"),
		"alice workspace":          filepath.Join(home, "agents", "alice"),
		"bob workspace":            filepath.Join(outside, "bob"),
		"cert_file":                filepath.Join(home, "tls", "own.crt"),
		"key_file":                 filepath.Join(home, "tls", "own.key"),
		"backup.dest":              filepath.Join(outside, "backups"),
		"model workspace":          filepath.Join(home, "cli-alice"),
		"model response_log_file":  filepath.Join(home, "logs", "responses.log"),
		"mcp env_file":             filepath.Join(home, "mcp.env"),
		"BaseDir()":                filepath.Join(home, "agents"),
		"ResolveCommonDir()":       filepath.Join(home, "shared"),
		"BackupDir()":              filepath.Join(outside, "backups"),
		"AgentSessionDirs()[0]":    filepath.Join(home, "agents", "alice", "sessions"),
		"AgentDirs()[1] (alice's)": filepath.Join(home, "agents", "alice"),
	}
	for k, w := range want {
		if full[k] != w {
			t.Errorf("loaded %s = %q, want %q", k, full[k], w)
		}
	}

	// Any save writes the stored form, and the running config keeps the
	// full paths.
	if err = store.Update(func(c *Config) error { c.Operator.Name = "Alice"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := store.Current().Agents.BaseDir; got != filepath.Join(home, "agents") {
		t.Errorf("after the save the running base_dir = %q, want the full path", got)
	}
	saved := readSaved(t, path)
	stored := map[string]string{
		"agents.base_dir":         saved.Agents.BaseDir,
		"agents.common_dir":       saved.Agents.CommonDir,
		"alice workspace":         saved.Agents.List[0].Workspace,
		"bob workspace":           saved.Agents.List[1].Workspace,
		"cert_file":               saved.Gateway.TLS.CertFile,
		"key_file":                saved.Gateway.TLS.KeyFile,
		"backup.dest":             saved.Backup.Dest,
		"model workspace":         saved.Models[0].Workspace,
		"model response_log_file": saved.Models[0].ResponseLogFile,
		"mcp env_file":            saved.Tools.MCP.Servers["files"].EnvFile,
	}
	wantStored := map[string]string{
		"agents.base_dir":         "agents",
		"agents.common_dir":       "shared",
		"alice workspace":         "agents/alice",
		"bob workspace":           filepath.Join(outside, "bob"),
		"cert_file":               "tls/own.crt",
		"key_file":                "tls/own.key",
		"backup.dest":             filepath.Join(outside, "backups"),
		"model workspace":         "cli-alice",
		"model response_log_file": "logs/responses.log",
		"mcp env_file":            "mcp.env",
	}
	for k, w := range wantStored {
		if stored[k] != w {
			t.Errorf("saved %s = %q, want %q", k, stored[k], w)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), home) {
		t.Errorf("the saved config still names the data directory:\n%s", raw)
	}
}

// A relative path a save brings in (the WebUI) names a place in the data
// directory: the running config holds the full path, the file the relative
// one.
func TestPaths_UpdateAcceptsRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	path := filepath.Join(home, "config.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) error {
		c.Agents.BaseDir = "elsewhere/agents"
		c.Models = append(c.Models, modelspec.Model{ModelName: "m2", Model: "x", Provider: "Claude CLI", Workspace: "cli2"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Current()
	if cfg.Agents.BaseDir != filepath.Join(home, "elsewhere", "agents") || cfg.BaseDir() != filepath.Join(home, "elsewhere", "agents") {
		t.Errorf("running base_dir = %q (BaseDir() %q), want it inside the data directory", cfg.Agents.BaseDir, cfg.BaseDir())
	}
	saved := readSaved(t, path)
	if saved.Agents.BaseDir != "elsewhere/agents" {
		t.Errorf("saved base_dir = %q, want elsewhere/agents", saved.Agents.BaseDir)
	}
	if m := saved.Models[len(saved.Models)-1]; m.Workspace != "cli2" {
		t.Errorf("saved model workspace = %q, want cli2", m.Workspace)
	}
}

// The seeded config names the agents folder relative to the data directory.
func TestPaths_SeededDefaultIsRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	path := filepath.Join(home, "config.json")
	if err := SeedDefaultConfig(path, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if saved := readSaved(t, path); saved.Agents.BaseDir != "agents" {
		t.Errorf("seeded base_dir = %q, want agents", saved.Agents.BaseDir)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.BaseDir != filepath.Join(home, "agents") {
		t.Errorf("loaded base_dir = %q, want the full path", cfg.Agents.BaseDir)
	}
}

// readSaved decodes the file as it is, without LoadConfig's resolution.
func readSaved(t *testing.T, path string) *Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}
