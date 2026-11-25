package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func requireWebUIOn(t *testing.T, w WebUIConfig) {
	t.Helper()
	if !w.Enabled || len(w.AllowFrom) != 1 || w.AllowFrom[0] != "*" {
		t.Errorf("webui = %+v, want enabled with allow_from [*]", w)
	}
	if b, err := hex.DecodeString(w.Token); err != nil || len(b) != 16 {
		t.Errorf("token %q is not 32 hex characters", w.Token)
	}
}

// TestNewInstallConfig: a new install has the WebUI chat channel on with a
// token of its own; DefaultConfig, the base existing files load over, keeps
// it off and holds no token.
func TestNewInstallConfig(t *testing.T) {
	a, b := NewInstallConfig(), NewInstallConfig()
	requireWebUIOn(t, a.Channels.WebUI)
	if a.Channels.WebUI.Token == b.Channels.WebUI.Token {
		t.Error("two new installs got the same token")
	}
	if d := DefaultConfig().Channels.WebUI; d.Enabled || d.Token != "" {
		t.Errorf("DefaultConfig webui = %+v, want off without a token", d)
	}
}

// TestLoadConfig_NewInstallAndExisting: a missing file loads as a new
// install; an existing file keeps its own WebUI setting, off included.
func TestLoadConfig_NewInstallAndExisting(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	requireWebUIOn(t, cfg.Channels.WebUI)

	p := filepath.Join(dir, "config.json")
	if werr := os.WriteFile(p, []byte(`{"channels": {"webui": {"enabled": false}}}`), 0o600); werr != nil {
		t.Fatal(werr)
	}
	cfg, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Channels.WebUI; w.Enabled || w.Token != "" {
		t.Errorf("existing config's webui = %+v, want it left off", w)
	}
}
