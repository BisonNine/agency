// Agency
// License: MIT

package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	acplib "github.com/a3tai/openclaw-go/acp"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

func TestNewHTTPHost_BadTrustedProxy(t *testing.T) {
	if _, err := newHTTPHost(hostOptions{TrustedProxies: []string{"not-a-proxy"}}); err == nil {
		t.Fatal("a bad trusted proxy was accepted")
	}
}

// TestHTTPHost_NoMuxYet: before the mux is mounted every request is 404.
func TestHTTPHost_NoMuxYet(t *testing.T) {
	h, err := newHTTPHost(hostOptions{HTTPHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := h.Stop(context.Background()); cerr != nil {
			t.Error(cerr)
		}
	})
	rec := &statusRecorder{header: http.Header{}}
	h.serveMux(rec, httpRequest(t, "/anything"))
	if rec.code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.code)
	}
}

type statusRecorder struct {
	header http.Header
	code   int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.code = code }

func httpRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestHTTPHost_HTTPSBindFailureClosesHTTP: when the HTTPS port cannot be
// bound the plain-HTTP listeners already opened are closed again.
func TestHTTPHost_HTTPSBindFailureClosesHTTP(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := taken.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	httpPort := freeLoopbackPort(t)
	takenAddr, ok := taken.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", taken.Addr())
	}
	tlsPort := takenAddr.Port
	h, err := newHTTPHost(hostOptions{HTTPHosts: []string{"127.0.0.1"}, Port: httpPort, TLSHosts: []string{"127.0.0.1"}, TLSPort: tlsPort})
	if err != nil {
		t.Fatal(err)
	}
	if startErr := h.Start(); startErr == nil || !strings.Contains(startErr.Error(), "bind HTTPS listener") {
		t.Fatalf("Start = %v", startErr)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort)))
	if err != nil {
		t.Fatalf("the HTTP port is still held: %v", err)
	}
	if cerr := ln.Close(); cerr != nil {
		t.Error(cerr)
	}
}

// TestBindAll_IPv6LoopbackOptional: an unavailable [::1] is skipped, not
// fatal, once another address is bound.
func TestBindAll_IPv6LoopbackOptional(t *testing.T) {
	v6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	defer func() {
		if cerr := v6.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	v6Addr, ok := v6.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", v6.Addr())
	}
	port := v6Addr.Port
	lns, got, err := bindAll("HTTP", []string{"127.0.0.1", "::1"}, port)
	if err != nil {
		t.Skipf("IPv4 port %d taken too: %v", port, err)
	}
	defer func() {
		for _, l := range lns {
			if cerr := l.Close(); cerr != nil {
				t.Error(cerr)
			}
		}
	}()
	if got != port || len(lns) != 1 {
		t.Errorf("bound %d listeners on %d, want just IPv4 on %d", len(lns), got, port)
	}
}

// failingListener fails every Accept with a non-closing error.
type failingListener struct{ addr net.Addr }

func (l failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept broke") }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr            { return l.addr }

// TestHTTPHost_ServeErrorWithoutFatal: a listener that dies is logged when no
// fatal handler is set.
func TestHTTPHost_ServeErrorWithoutFatal(t *testing.T) {
	h := &httpHost{}
	ln := failingListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}}
	out := logLines(t, func() { h.serve(&http.Server{ReadHeaderTimeout: time.Second}, ln, false) })
	if !strings.Contains(out, "HTTP listener error") {
		t.Errorf("log = %s", out)
	}
}

// TestHTTPHost_StopTimesOut: a request still running when the stop deadline
// passes makes Stop report it.
func TestHTTPHost_StopTimesOut(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	h, err := newHTTPHost(hostOptions{HTTPHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	})
	h.SetMux(mux)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		resp, err := http.Get("http://" + h.LoopbackAddr() + "/health")
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Error(cerr)
			}
		}
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Stop(ctx); err == nil {
		t.Error("Stop with a request in flight and no time left succeeded")
	}
	close(release)
	if cerr := h.Stop(context.Background()); cerr != nil {
		t.Error(cerr)
	}
}

// TestSetupAndStartServices_Errors: listener settings the start cannot use
// fail it with the reason.
func TestSetupAndStartServices_Errors(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := taken.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	tests := []struct {
		name   string
		mutate func(cfg *config.Config)
		want   string
	}{
		{"bad allowlist", func(c *config.Config) { c.Gateway.AllowedCIDRs = []string{"not-a-cidr"} }, "invalid network allowlist"},
		{"bad external url", func(c *config.Config) { c.Gateway.ExternalURL = "http://[::1" }, ""},
		{"bad lockout exemption", func(c *config.Config) { c.Gateway.LockoutExempt = []string{"not an address"} }, ""},
		{"MCP host port taken", func(c *config.Config) {
			c.MCPHost.Enabled = true
			c.MCPHost.Listen = taken.Addr().String()
		}, "MCP server"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newE2EHome(t, "")
			cfgPath := filepath.Join(g.home, "config.json")
			store, cfg, _, err := openBootConfig(g.home, cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			msgBus := bus.NewMessageBus()
			al, err := newGatewayAgentLoop(cfg, msgBus, agent.NewForumHost(), agent.NewClassifierHost(), &alertRecorder{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { al.Close(context.Background()) })
			before := countServiceGoroutines()
			services, err := setupAndStartServices(cfg, al, msgBus, cfgPath, store, newFatalNotifier(&alertRecorder{}))
			if err == nil {
				shutdownGateway(services, al, false)
				t.Fatal("the services started")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			// What the start got to is stopped again.
			requireNoNewServiceGoroutines(t, before)
			requirePortFree(t, g.port)
		})
	}
}

// TestGatewayCmd_AgentsUnusable: agents whose folder cannot be created stop
// the start.
func TestGatewayCmd_AgentsUnusable(t *testing.T) {
	g := newE2EHome(t, "")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g.rewriteConfig(t, func(s string) string {
		return strings.Replace(s, filepath.Join(g.home, "agents"), filepath.Join(blocker, "agents"), 1)
	})
	if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), "error creating agent loop") {
		t.Fatalf("err = %v", err)
	}
}

// TestGatewayCmd_NoModel: a gateway whose default model is not configured
// starts unconfigured, with a warning, and stops cleanly.
func TestGatewayCmd_NoModel(t *testing.T) {
	g := newE2EHome(t, "")
	g.rewriteConfig(t, func(s string) string {
		return strings.Replace(s, `"models": ["m"]`, `"models": ["missing-model"]`, 1)
	})
	g.start(t)
	g.login(t)
	if code, _ := g.post(t, "/api/system/restart"); code != http.StatusAccepted {
		t.Fatalf("restart = %d", code)
	}
	if err := g.wait(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("gatewayCmd = %v", err)
	}
}

// TestSetupCronTool_UnreadableStoreAlerts: a cron store that cannot be read
// is set aside when the cron service starts, and reported once.
func TestSetupCronTool_UnreadableStoreAlerts(t *testing.T) {
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	cfg := config.DefaultConfig()
	cfg.Agents.BaseDir = t.TempDir()
	cfg.Agents.Defaults.Models = []string{"test-model"}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Default: true}}
	if err := os.MkdirAll(cfg.CronPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CronPath(), "jobs.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close(context.Background()) })
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	svc, _ := setupCronTool(al, bus.NewMessageBus(), cfg)
	if len(rec.alerts) != 0 {
		t.Fatalf("alerts before Start = %v", rec.titles())
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(svc.Stop)
	if len(rec.alerts) != 1 || !rec.has("Cron store unreadable") {
		t.Fatalf("alerts = %v", rec.titles())
	}
	if m, gerr := filepath.Glob(filepath.Join(cfg.CronPath(), "jobs.json.unreadable-*")); gerr != nil || len(m) != 1 {
		t.Errorf("set-aside files = %v, want one", m)
	}
}

// TestSetupCogmem_NoCognitiveAgent: without a cognitive agent no
// consolidation manager is started.
func TestSetupCogmem_NoCognitiveAgent(t *testing.T) {
	off := false
	cfg := config.DefaultConfig()
	cfg.Agents.BaseDir = t.TempDir()
	cfg.Agents.Defaults.Models = []string{"test-model"}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Default: true, Cogmem: &off}}
	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close(context.Background()) })
	if mgr := setupCogmemConsolidation(cfg, al); mgr != nil {
		t.Fatal("a manager was started without a cognitive agent")
	}
}

func TestACPPromptAttachments_Named(t *testing.T) {
	name := "photo.png"
	got := acpPromptAttachments([]acplib.ContentBlock{{Type: "image", MimeType: "image/png", Data: "AAAA", Name: &name}, {Type: "image"}})
	if len(got) != 1 || got[0].Name != "photo.png" {
		t.Fatalf("attachments = %+v", got)
	}
}

func TestPruneOldLogs_Edges(t *testing.T) {
	pruneOldLogs(filepath.Join(t.TempDir(), "missing"), 7, time.Now()) // no folder: nothing to do
	dir := t.TempDir()
	odd := filepath.Join(dir, "99999999-agency.log") // dated-looking but no date
	if err := os.WriteFile(odd, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pruneOldLogs(dir, 7, time.Now())
	if _, err := os.Stat(odd); err != nil {
		t.Errorf("an undated file was pruned: %v", err)
	}
	if os.Geteuid() != 0 {
		old := filepath.Join(dir, "20000101-agency.log")
		if err := os.WriteFile(old, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(dir, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if out := logLines(t, func() { pruneOldLogs(dir, 7, time.Now()) }); !strings.Contains(out, "failed to prune rolled log") {
			t.Errorf("log = %s", out)
		}
	}
}

// TestGatewayCmd_DegradedStart: a pid file, log folder and common folder the
// gateway cannot write are warned about and the gateway still serves.
func TestGatewayCmd_DegradedStart(t *testing.T) {
	g := newE2EHome(t, "")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g.rewriteConfig(t, func(s string) string {
		return strings.Replace(s, `"base_dir": `, `"common_dir": "`+filepath.Join(blocker, "common")+`", "base_dir": `, 1)
	})
	if err := os.MkdirAll(filepath.Join(g.home, "internal", "agency.pid", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.home, "logs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g.start(t)
	g.login(t)
	if code, _ := g.post(t, "/api/system/restart"); code != http.StatusAccepted {
		t.Fatalf("restart = %d", code)
	}
	if err := g.wait(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("gatewayCmd = %v", err)
	}
}

// TestLogHealthEndpoints_Network: a network bind names its network URLs.
func TestLogHealthEndpoints_Network(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Gateway.Host = "0.0.0.0"
	services := &gatewayServices{HTTPHost: &httpHost{loopbackAddr: "127.0.0.1:1"}}
	if out := logLines(t, func() { logHealthEndpoints(services, cfg) }); !strings.Contains(out, "http_network") {
		t.Errorf("log = %s", out)
	}
}

// TestACPCmd_MoreErrors: with a log folder that cannot be written, a broken
// stored identity stops the bridge.
func TestACPCmd_MoreErrors(t *testing.T) {
	t.Run("identity unreadable", func(t *testing.T) {
		g := startACPTestGateway(t, t.TempDir(), false)
		acpHome(t, g, "")
		idDir := filepath.Join(g.dataDir, "internal", "acp-bridge")
		if err := os.MkdirAll(filepath.Join(idDir, "keypair.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.dataDir, "logs"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		withACPStdio(t)
		if err := acpCmd(false, "", true); err == nil || !strings.Contains(err.Error(), "acp: load device identity") {
			t.Fatalf("err = %v", err)
		}
	})
}
