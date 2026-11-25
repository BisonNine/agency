// Agency
// License: MIT

package gateway

import (
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.bison9.com/agency/config"
)

// TestGatewayCmd_NewInstallStartsWithWebUIChat: a data directory with no
// config.json (a new install, package or interactive) is seeded with the
// WebUI chat channel on and a generated token, so the gateway starts, stays
// up and the admin can sign in; "no channels" (TestGatewayCmd_MoreStartupErrors)
// is only a config whose every channel is off.
func TestGatewayCmd_NewInstallStartsWithWebUIChat(t *testing.T) {
	g := newE2EHome(t, "")
	cfgPath := filepath.Join(g.home, "config.json")
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	// Only what keeps the test off the machine's own ports; the seeded
	// channels are left as they are.
	t.Setenv("AGENCY_GATEWAY_PORT", strconv.Itoa(g.port))
	t.Setenv("AGENCY_GATEWAY_TLS_MODE", "off")
	t.Setenv("AGENCY_MCP_HOST_ENABLED", "false")
	t.Setenv("AGENCY_MCP_HOST_AUTO_ENABLE", "false")

	g.start(t)
	g.login(t)
	if code, _ := g.post(t, "/api/gateway/reload"); code != http.StatusOK {
		t.Errorf("reload = %d", code)
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Channels.WebUI
	if !w.Enabled || len(w.AllowFrom) != 1 || w.AllowFrom[0] != "*" {
		t.Errorf("seeded webui channel = %+v", w)
	}
	if b, herr := hex.DecodeString(w.Token); herr != nil || len(b) != 16 {
		t.Errorf("seeded webui token %q is not 32 hex characters", w.Token)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("config.json mode = %o, want 600", m)
	}
	(<-g.signals) <- os.Interrupt
	if err := g.wait(t); err != nil {
		t.Fatalf("gateway after SIGINT = %v", err)
	}
}
