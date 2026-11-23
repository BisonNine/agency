package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
)

// badConfig points CLAW_HOME at a config that does not parse.
func badConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableConfigIsAnError(t *testing.T) {
	badConfig(t)
	if _, err := CurrentAllowlist(); err == nil {
		t.Error("CurrentAllowlist accepted a broken config")
	}
	if _, err := ApplyAllowlist([]string{"10.0.0.0/8"}); err == nil {
		t.Error("ApplyAllowlist accepted a broken config")
	}
	if _, err := ApplyListeners(Listeners{HTTP: ScopeNetwork}); err == nil {
		t.Error("ApplyListeners accepted a broken config")
	}
	if _, err := CurrentAccess(); err == nil {
		t.Error("CurrentAccess accepted a broken config")
	}
	if _, err := runCommand(t, "--show"); err == nil {
		t.Error("--show accepted a broken config")
	}
}

func TestApplySaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	path := seedConfig(t)
	before := readFile(t, path)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := ApplyListeners(Listeners{HTTP: ScopeNetwork}); err == nil {
		t.Fatal("ApplyListeners reported success without writing")
	}
	if _, err := ApplyAllowlist([]string{"10.0.0.0/8"}); err == nil {
		t.Fatal("ApplyAllowlist reported success without writing")
	}
	if string(readFile(t, path)) != string(before) {
		t.Error("the config changed")
	}
}

func TestShowHTTPSScopes(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{config.TLSModeOff, "HTTPS:             off\n"},
		{config.TLSModeLocalhost, "HTTPS:             localhost (port "},
	} {
		path := seedConfig(t)
		cfg := loadSeeded(t)
		cfg.Gateway.TLS.Mode = tc.mode
		if err := config.SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		out, err := runCommand(t, "--show")
		if err != nil {
			t.Fatalf("--show: %v", err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("mode %s: output lacks %q:\n%s", tc.mode, tc.want, out)
		}
	}
}
