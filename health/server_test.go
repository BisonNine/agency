package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/health"
	"go.bison9.com/agency/logger"
)

// closeT closes c and fails the test if that fails.
func closeT(tb testing.TB, c io.Closer) {
	tb.Helper()
	if err := c.Close(); err != nil {
		tb.Errorf("close: %v", err)
	}
}

// closeBody closes resp's body and fails the test if that fails.
func closeBody(tb testing.TB, resp *http.Response) {
	tb.Helper()
	if err := resp.Body.Close(); err != nil {
		tb.Errorf("close body: %v", err)
	}
}

// serve mounts s on a fresh mux behind an httptest server.
func serve(t *testing.T, s *health.Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterOnMux(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// get fetches path and decodes the JSON body.
func get(t *testing.T, ts *httptest.Server, path string) (int, health.StatusResponse) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer closeBody(t, resp)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
	}
	var body health.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decode body: %v", path, err)
	}
	return resp.StatusCode, body
}

func TestHealthReportsOKWhetherOrNotReady(t *testing.T) {
	s := health.NewServer("127.0.0.1", 0)
	ts := serve(t, s)

	for _, ready := range []bool{false, true} {
		s.SetReady(ready)
		code, body := get(t, ts, "/health")
		if code != http.StatusOK {
			t.Errorf("ready=%v: status = %d, want 200", ready, code)
		}
		if body.Status != "ok" {
			t.Errorf("ready=%v: body status = %q, want ok", ready, body.Status)
		}
		if _, err := time.ParseDuration(body.Uptime); err != nil {
			t.Errorf("ready=%v: uptime %q is not a duration: %v", ready, body.Uptime, err)
		}
		if body.Checks != nil {
			t.Errorf("ready=%v: health carries checks %v, want none", ready, body.Checks)
		}
	}
}

func TestReady(t *testing.T) {
	pass := func() (bool, string) { return true, "" }
	fail := func() (bool, string) { return false, "database unreachable" }

	tests := []struct {
		name       string
		ready      bool
		checks     map[string]func() (bool, string)
		wantCode   int
		wantStatus string
		wantUptime bool
	}{
		{
			name:       "new server is not ready",
			ready:      false,
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not ready",
		},
		{
			name:       "ready with no checks",
			ready:      true,
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			wantUptime: true,
		},
		{
			name:       "ready with passing checks",
			ready:      true,
			checks:     map[string]func() (bool, string){"db": pass, "cache": pass},
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			wantUptime: true,
		},
		{
			name:       "ready but a check fails",
			ready:      true,
			checks:     map[string]func() (bool, string){"db": fail, "cache": pass},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not ready",
		},
		{
			name:       "not ready with passing checks",
			ready:      false,
			checks:     map[string]func() (bool, string){"db": pass},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := health.NewServer("127.0.0.1", 0)
			for name, fn := range tt.checks {
				s.RegisterCheck(name, fn)
			}
			s.SetReady(tt.ready)
			ts := serve(t, s)

			code, body := get(t, ts, "/ready")
			if code != tt.wantCode {
				t.Errorf("status = %d, want %d", code, tt.wantCode)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("body status = %q, want %q", body.Status, tt.wantStatus)
			}
			if gotUptime := body.Uptime != ""; gotUptime != tt.wantUptime {
				t.Errorf("uptime = %q, want present=%v", body.Uptime, tt.wantUptime)
			}
			if len(body.Checks) != len(tt.checks) {
				t.Fatalf("checks = %v, want %d entries", body.Checks, len(tt.checks))
			}
			for name, fn := range tt.checks {
				ok, msg := fn()
				want := "fail"
				if ok {
					want = "ok"
				}
				got, found := body.Checks[name]
				if !found {
					t.Errorf("check %q missing from %v", name, body.Checks)
					continue
				}
				if got.Name != name || got.Status != want || got.Message != msg {
					t.Errorf("check %q = %+v, want name %q status %q message %q", name, got, name, want, msg)
				}
				if got.Timestamp.IsZero() {
					t.Errorf("check %q has no timestamp", name)
				}
			}
		})
	}
}

func TestRegisterCheckReplacesEarlierResult(t *testing.T) {
	s := health.NewServer("127.0.0.1", 0)
	s.SetReady(true)
	s.RegisterCheck("db", func() (bool, string) { return false, "down" })
	ts := serve(t, s)

	if code, _ := get(t, ts, "/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("with failing check: status = %d, want 503", code)
	}

	s.RegisterCheck("db", func() (bool, string) { return true, "" })
	code, body := get(t, ts, "/ready")
	if code != http.StatusOK {
		t.Errorf("after re-registering as passing: status = %d, want 200", code)
	}
	if got := body.Checks["db"]; got.Status != "ok" || got.Message != "" {
		t.Errorf("db check = %+v, want status ok with no message", got)
	}
}

func TestSetReadyTogglesReadiness(t *testing.T) {
	s := health.NewServer("127.0.0.1", 0)
	ts := serve(t, s)

	steps := []struct {
		ready bool
		want  int
	}{
		{true, http.StatusOK},
		{false, http.StatusServiceUnavailable},
		{true, http.StatusOK},
	}
	for _, step := range steps {
		s.SetReady(step.ready)
		if code, _ := get(t, ts, "/ready"); code != step.want {
			t.Errorf("SetReady(%v): status = %d, want %d", step.ready, code, step.want)
		}
	}
}

func TestHandleRegistersOnExternalMux(t *testing.T) {
	s := health.NewServer("127.0.0.1", 0)
	ts := serve(t, s) // RegisterOnMux happens before Handle

	s.Handle("/extra", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	resp, err := ts.Client().Get(ts.URL + "/extra")
	if err != nil {
		t.Fatalf("GET /extra: %v", err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

// freePort returns a port the kernel picked and has just released.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := tcpPort(t, ln)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

// busyPort returns a port held open by a listener for the rest of the test.
func busyPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { closeT(t, ln) })
	return tcpPort(t, ln)
}

// tcpPort returns the port ln listens on.
func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", ln.Addr())
	}
	return addr.Port
}

// plainClient opens a fresh connection per request, so no idle keep-alive
// connection is left for Shutdown to wait on.
var plainClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// waitServing polls /ready on port until the server answers, so the test
// knows the listener is up before it stops it.
func waitServing(t *testing.T, port int) {
	t.Helper()
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/ready"
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := plainClient.Get(url)
		if err == nil {
			closeBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("started server /ready = %d, want 200", resp.StatusCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server on port %d never answered: %v", port, err)
		}
		runtime.Gosched()
	}
}

func TestStartServesAndStopShutsDown(t *testing.T) {
	port := freePort(t)
	s := health.NewServer("127.0.0.1", port)
	s.Handle("/extra", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	waitServing(t, port) // Start marks the server ready

	// A handler added with Handle is served by the server's own listener.
	resp, err := plainClient.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/extra")
	if err != nil {
		t.Fatalf("GET /extra: %v", err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("GET /extra = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Start returned %v, want http.ErrServerClosed", err)
	}
}

func TestStartReportsListenError(t *testing.T) {
	s := health.NewServer("127.0.0.1", busyPort(t))
	if err := s.Start(); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Start on a busy port returned %v, want a listen error", err)
	}
}

func TestStartContextShutsDownOnCancel(t *testing.T) {
	port := freePort(t)
	s := health.NewServer("127.0.0.1", port)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- s.StartContext(ctx) }()
	waitServing(t, port)

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("StartContext after cancel returned %v, want nil", err)
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/health"
	if resp, err := plainClient.Get(url); err == nil {
		closeBody(t, resp)
		t.Errorf("server still answering after StartContext returned")
	}
}

func TestStartContextReportsListenError(t *testing.T) {
	s := health.NewServer("127.0.0.1", busyPort(t))
	err := s.StartContext(context.Background())
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("StartContext on a busy port returned %v, want a listen error", err)
	}
}

func TestStopMarksNotReady(t *testing.T) {
	s := health.NewServer("127.0.0.1", 0)
	s.SetReady(true)
	ts := serve(t, s)

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on a server that never started: %v", err)
	}
	if code, body := get(t, ts, "/ready"); code != http.StatusServiceUnavailable || body.Status != "not ready" {
		t.Errorf("after Stop: /ready = %d %q, want 503 not ready", code, body.Status)
	}
}

// brokenWriter accepts headers but fails every body write, as a connection
// the client has dropped does.
type brokenWriter struct {
	header http.Header
	code   int
}

func (w *brokenWriter) Header() http.Header       { return w.header }
func (w *brokenWriter) WriteHeader(code int)      { w.code = code }
func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestFailedResponseWriteIsLogged(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "health.log")
	if err := logger.EnableFileLogging(logPath, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.DisableFileLogging)

	s := health.NewServer("127.0.0.1", 0)
	mux := http.NewServeMux()
	s.RegisterOnMux(mux)

	w := &brokenWriter{header: http.Header{}}
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.code)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "response write failed") || !strings.Contains(string(data), "connection reset") {
		t.Errorf("log does not record the failed write:\n%s", data)
	}
}
