// ClawEh
// License: MIT

package gateway

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// TestGatewayCmd_MoreStartupErrors: a loose config file, a config that is not
// a file, no channel to run and a missing certificate each stop the start
// with the reason, and leave nothing running.
func TestGatewayCmd_MoreStartupErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, g *e2eGateway)
		want  string
	}{
		{"config readable by others", func(t *testing.T, g *e2eGateway) {
			t.Helper()
			if err := os.Chmod(filepath.Join(g.home, "config.json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "startup aborted"},
		{"config is a folder", func(t *testing.T, g *e2eGateway) {
			t.Helper()
			p := filepath.Join(g.home, "config.json")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "error loading config"},
		{"no channels", func(t *testing.T, g *e2eGateway) {
			t.Helper()
			g.rewriteConfig(t, func(s string) string {
				return strings.Replace(s, `"webui": {"enabled": true, "token": "e2e-webui-token"}`, `"webui": {"enabled": false}`, 1)
			})
		}, "error starting channels"},
		{"certificate files missing", func(t *testing.T, g *e2eGateway) {
			t.Helper()
			missing := filepath.Join(t.TempDir(), "missing")
			g.rewriteConfig(t, func(s string) string {
				return strings.Replace(s, `"tls": {"mode": "off"}`,
					`"tls": {"mode": "localhost", "cert_file": "`+missing+`.crt", "key_file": "`+missing+`.key"}, "tls_port": 1`, 1)
			})
		}, "HTTPS listener certificate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newE2EHome(t, "")
			tc.setup(t, g)
			before := countServiceGoroutines()
			if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			// A failed start leaves nothing running.
			requireNoNewServiceGoroutines(t, before)
			requirePortFree(t, g.port)
		})
	}
}

// TestGatewayCmd_ForceReloadErrors: the reload API reports a config that does
// not parse and a reload that cannot restart the MCP host; a good config
// reloads again afterwards.
func TestGatewayCmd_ForceReloadErrors(t *testing.T) {
	g := newE2EHome(t, "")
	g.start(t)
	g.login(t)
	cfgPath := filepath.Join(g.home, "config.json")
	good, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if ierr := os.WriteFile(cfgPath, []byte(`{"models": [`), 0o600); ierr != nil {
		t.Fatal(ierr)
	}
	if code, body := g.post(t, "/api/gateway/reload"); code != http.StatusInternalServerError {
		t.Errorf("reload of an unparseable config = %d %s", code, body)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := taken.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	var doc map[string]any
	if ierr := json.Unmarshal(good, &doc); ierr != nil {
		t.Fatal(ierr)
	}
	doc["mcp_host"] = map[string]any{"enabled": true, "listen": taken.Addr().String()}
	withHost, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, withHost, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := g.post(t, "/api/gateway/reload"); code != http.StatusInternalServerError || !strings.Contains(body, "MCP server") {
		t.Errorf("reload with the MCP host port taken = %d %s", code, body)
	}

	if err := os.WriteFile(cfgPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := g.post(t, "/api/gateway/reload"); code != http.StatusOK {
		t.Errorf("reload of the good config = %d %s", code, body)
	}
	g.waitReady(t)
	if code, _ := g.post(t, "/api/system/restart"); code != http.StatusAccepted {
		t.Fatalf("restart = %d", code)
	}
	if err := g.wait(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("gatewayCmd = %v", err)
	}
}

// TestGatewayCmd_QuietLogging: with file and console logging off the gateway
// runs and writes no log file.
func TestGatewayCmd_QuietLogging(t *testing.T) {
	g := newE2EHome(t, "")
	g.rewriteConfig(t, func(s string) string {
		return strings.Replace(s, `"logging": {"console": true, "file": true, "level": "warn"}`, `"logging": {"console": false, "file": false}`, 1)
	})
	// The gateway silences the console; give it back afterwards.
	restore := logger.RedirectForTest(io.Discard)
	t.Cleanup(restore)
	g.start(t)
	g.login(t)
	if code, _ := g.post(t, "/api/system/restart"); code != http.StatusAccepted {
		t.Fatalf("restart = %d", code)
	}
	if err := g.wait(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("gatewayCmd = %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.home, "logs", "claw.log")); err == nil {
		if b, rerr := os.ReadFile(filepath.Join(g.home, "logs", "claw.log")); rerr != nil || strings.Contains(string(b), "Shutting down") {
			t.Error("claw.log written with file logging off")
		}
	}
}

// TestListenerWarnings: an off-box bind without an allowlist warns, and a
// gateway without listeners has nothing to compare a reloaded bind with.
func TestListenerWarnings(t *testing.T) {
	out := logLines(t, func() { logAllowlist(nil, "0.0.0.0") })
	if !strings.Contains(out, "the network allowlist is empty") {
		t.Errorf("log = %s", out)
	}
	if out := logLines(t, func() { logAllowlist(nil, "127.0.0.1") }); strings.Contains(out, "allowlist is empty") {
		t.Errorf("loopback warned: %s", out)
	}
	warnListenerConfigChanged(&gatewayServices{}, config.GatewayConfig{Port: 1})
	if ch := setReloadTrigger(&gatewayServices{}); ch == nil || cap(ch) != 1 {
		t.Errorf("reload trigger without a web server = %v", ch)
	}
}
