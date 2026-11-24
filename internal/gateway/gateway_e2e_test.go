// Agency
// License: MIT

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/admin"
	"go.bison9.com/agency/internal/pidfile"
	"go.bison9.com/agency/internal/test/stubprovider"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/utils"
)

// e2eGateway is a whole gateway (gatewayCmd) running in this process on a
// temp AGENCY_HOME: a stub model, one agent, plain HTTP on a free loopback
// port, an admin account, no operator alert channels and no real process
// exit.
type e2eGateway struct {
	home   string
	port   int
	stub   *stubprovider.Server
	client *http.Client
	done   chan error
	exits  *exitRecorder
	// signals is the channel the main loop asked to be notified on.
	signals chan chan<- os.Signal
}

// exitRecorder stands in for os.Exit.
type exitRecorder struct {
	mu    sync.Mutex
	codes []int
}

func (r *exitRecorder) exit(code int) {
	r.mu.Lock()
	r.codes = append(r.codes, code)
	r.mu.Unlock()
}

const (
	e2eAdmin    = "alice"
	e2ePassword = "alice's long password"
)

// e2eConfig is the gateway config: extra is spliced into the top-level object.
func e2eConfig(t *testing.T, home, stubURL string, port int, extra string) string {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "gateway": {"host": "127.0.0.1", "port": %d, "tls": {"mode": "off"}},
  "logging": {"console": true, "file": true, "level": "warn"},
  "channels": {"webui": {"enabled": true, "token": "e2e-webui-token"}},
  "providers": [{"name": "stub", "protocol": "openai-chat", "base_url": %q, "api_key": "k"}],
  "models": [{"model_name": "m", "model": "stub-model", "provider": "stub", "enabled": true}],
  "agents": {
    "base_dir": %q,
    "defaults": {"models": ["m"], "request_timeout": 5, "progress_interval": -1},
    "list": [{"id": "main", "default": true}]
  }%s
}`, port, stubURL, filepath.Join(home, "agents"), extra)
	return cfg
}

// newE2EHome prepares AGENCY_HOME with the config and admin account, and
// isolates the process state a gateway touches.
func newE2EHome(t *testing.T, extra string) *e2eGateway {
	t.Helper()
	g := &e2eGateway{
		home:    t.TempDir(),
		port:    freeLoopbackPort(t),
		stub:    stubprovider.New(t),
		done:    make(chan error, 1),
		exits:   &exitRecorder{},
		signals: make(chan chan<- os.Signal, 1),
	}
	t.Setenv(global.EnvVarHome, g.home)
	// No ~/.alerter and no alert channel: alerts land in the home's alerts.log.
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		"ALERTER_PUSHOVER_TOKEN", "ALERTER_PUSHOVER_DEST", "ALERTER_PUSHOVER_NORMAL", "ALERTER_PUSHOVER_URGENT",
		"ALERTER_PUSHOVER_EMERGENCY", "ALERTER_TELNYX_API_KEY", "ALERTER_SMS_FROM", "ALERTER_SMS_TO",
		"ALERTER_SMTP_HOST", "ALERTER_SMTP_PORT", "ALERTER_SMTP_USER", "ALERTER_SMTP_PASSWORD",
		"ALERTER_SMTP_FROM", "ALERTER_SMTP_TO", "ALERTER_WEBHOOK_URL", "ALERTER_WEBHOOK_HEADERS", "ALERTER_LOG",
	} {
		t.Setenv(k, "")
	}
	// POST /api/system/restart answers only under a service manager.
	t.Setenv("INVOCATION_ID", "e2e")

	if err := os.WriteFile(filepath.Join(g.home, "config.json"), []byte(e2eConfig(t, g.home, g.stub.URL(), g.port, extra)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := admin.Write(admin.Path(g.home), e2eAdmin, e2ePassword, admin.DefaultFileMode); err != nil {
		t.Fatal(err)
	}

	origExit, origNotify, origLevel := processExit, notifySignals, logger.GetLevel()
	processExit = g.exits.exit
	notifySignals = func(c chan<- os.Signal, _ ...os.Signal) { g.signals <- c }
	t.Cleanup(func() {
		processExit, notifySignals = origExit, origNotify
		logger.DisableFileLogging()
		logger.SetLevel(origLevel)
		utils.SetDisableTruncation(false)
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	g.client = &http.Client{Jar: jar, Timeout: 30 * time.Second}
	return g
}

// rewriteConfig passes config.json through mutate.
func (g *e2eGateway) rewriteConfig(t *testing.T, mutate func(string) string) {
	t.Helper()
	p := filepath.Join(g.home, "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(mutate(string(b))), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (g *e2eGateway) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", g.port, path)
}

// start runs gatewayCmd and waits until /ready answers 200.
func (g *e2eGateway) start(t *testing.T) {
	t.Helper()
	go func() { g.done <- gatewayCmd(false) }()
	g.waitReady(t)
}

// waitReady polls /ready until it answers 200, failing if the gateway exits.
func (g *e2eGateway) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-g.done:
			t.Fatalf("gateway exited during startup: %v", err)
		default:
		}
		resp, err := g.client.Get(g.url("/ready"))
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Error(cerr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond) // polling a condition, not timing
	}
	t.Fatal("gateway not ready within 60s")
}

// wait returns gatewayCmd's result.
func (g *e2eGateway) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-g.done:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("gateway did not stop within 90s")
		return nil
	}
}

func (g *e2eGateway) login(t *testing.T) {
	t.Helper()
	body, rerr := json.Marshal(map[string]string{"username": e2eAdmin, "password": e2ePassword})
	if rerr != nil {
		t.Fatal(rerr)
	}
	resp, err := g.client.Post(g.url("/api/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if cerr := resp.Body.Close(); cerr != nil {
		t.Error(cerr)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login = %d", resp.StatusCode)
	}
}

func (g *e2eGateway) post(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := g.client.Post(g.url(path), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	var b bytes.Buffer
	if _, cerr := b.ReadFrom(resp.Body); cerr != nil {
		t.Error(cerr)
	}
	return resp.StatusCode, b.String()
}

// TestGatewayCmd_RunReloadRestart runs the whole gateway: it starts, writes
// its pid file, answers /ready and the logged-in API, applies a forced config
// reload, and on POST /api/system/restart shuts down cleanly and returns the
// restart for the service manager (exit code 3), removing its pid and lock
// files.
func TestGatewayCmd_RunReloadRestart(t *testing.T) {
	g := newE2EHome(t, "")
	before := countServiceGoroutines()
	g.start(t)

	if pid, _ := pidfile.Read(g.home); pid != os.Getpid() {
		t.Errorf("pid file = %d; want this process", pid)
	}
	if running, err := lockHeld(g.home); err != nil || !running {
		t.Errorf("lock held = %v, %v; want held while running", running, err)
	}

	g.login(t)

	// Edit the config and apply it at once through the API.
	cfgPath := filepath.Join(g.home, "config.json")
	edited := e2eConfig(t, g.home, g.stub.URL(), g.port, `, "gateway_note_for_reload": true`)
	edited = strings.Replace(edited, `"level": "warn"`, `"level": "error"`, 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := g.post(t, "/api/gateway/reload"); code != http.StatusOK || !strings.Contains(body, "reloaded") {
		t.Fatalf("reload = %d %s", code, body)
	}
	g.waitReady(t) // ready again after the reload

	if code, body := g.post(t, "/api/system/restart"); code != http.StatusAccepted {
		t.Fatalf("restart = %d %s", code, body)
	}
	err := g.wait(t)
	var restart *restartRequestedError
	if !errors.As(err, &restart) || exitCode(err) != exitCodeServiceDied {
		t.Fatalf("gatewayCmd = %v, want the restart request (exit %d)", err, exitCodeServiceDied)
	}
	if _, perr := os.Stat(pidfile.Path(g.home)); !errors.Is(perr, os.ErrNotExist) {
		t.Errorf("pid file left behind: %v", perr)
	}
	if running, lerr := lockHeld(g.home); lerr != nil || running {
		t.Errorf("lock still held after shutdown: %v, %v", running, lerr)
	}
	// The shutdown stops every service loop, the backup scheduler included.
	requireNoNewServiceGoroutines(t, before)
	if _, serr := os.Stat(filepath.Join(g.home, "logs", global.LogFileName)); serr != nil {
		t.Errorf("no log file written: %v", serr)
	}
}

// lockHeld reports whether the gateway's lock in home is held.
func lockHeld(home string) (bool, error) {
	f, err := acquireLock(home)
	if err != nil {
		if strings.Contains(err.Error(), "another instance is already running") {
			return true, nil
		}
		return false, err
	}
	releaseLock(f)
	return false, nil
}

// TestGatewayCmd_Signal: SIGINT or SIGTERM shuts the gateway down cleanly and
// gatewayCmd returns no error, so the command exits 0.
func TestGatewayCmd_Signal(t *testing.T) {
	g := newE2EHome(t, "")
	cmd := NewGatewayCommand()
	cmd.SetArgs([]string{"-d", "-T"})
	go func() { g.done <- cmd.Execute() }()
	g.waitReady(t)
	sig := <-g.signals
	sig <- os.Interrupt
	if err := g.wait(t); err != nil {
		t.Fatalf("gateway after SIGINT = %v, want a clean exit", err)
	}
	if logger.GetLevel() != logger.DEBUG {
		t.Error("-d did not set the debug level")
	}
}

// TestGatewayCmd_StartupErrors: a gateway that cannot start returns why.
func TestGatewayCmd_StartupErrors(t *testing.T) {
	t.Run("port taken", func(t *testing.T) {
		g := newE2EHome(t, "")
		startHost(t, hostOptions{Port: g.port}, "taken")
		before := countServiceGoroutines()
		if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), "bind HTTP listener") {
			t.Fatalf("err = %v, want the bind failure", err)
		}
		requireNoNewServiceGoroutines(t, before)
	})
	t.Run("already running", func(t *testing.T) {
		g := newE2EHome(t, "")
		lock, err := acquireLock(g.home)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseLock(lock)
		if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), "startup aborted: another instance is already running") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("config unreadable", func(t *testing.T) {
		g := newE2EHome(t, "")
		if err := os.WriteFile(filepath.Join(g.home, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), "error loading config") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no default agent", func(t *testing.T) {
		g := newE2EHome(t, "")
		body := strings.Replace(e2eConfig(t, g.home, g.stub.URL(), g.port, ""), `"list": [{"id": "main", "default": true}]`, `"list": []`, 1)
		if err := os.WriteFile(filepath.Join(g.home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := gatewayCmd(false); err == nil || !strings.Contains(err.Error(), "no default agent configured") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestNewGatewayCommand_NoTruncateNeedsDebug: --no-truncate alone is refused
// before anything starts.
func TestNewGatewayCommand_NoTruncateNeedsDebug(t *testing.T) {
	cmd := NewGatewayCommand()
	cmd.SetArgs([]string{"--no-truncate"})
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--no-truncate option can only be used in conjunction with --debug") {
		t.Fatalf("err = %v", err)
	}
}
