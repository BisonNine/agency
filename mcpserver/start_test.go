// ClawEh
// License: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/toolkit"
)

// stubListener is a listener whose Accept fails when the test says so, and
// whose Close may report an error.
type stubListener struct {
	fail      chan error
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newStubListener(closeErr error) *stubListener {
	return &stubListener{fail: make(chan error, 1), closed: make(chan struct{}), closeErr: closeErr}
}

func (l *stubListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.fail:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *stubListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.closeErr
}

func (l *stubListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// recordingAlerter records the alerts it is sent.
type recordingAlerter struct{ sent chan alerter.Alert }

func (a *recordingAlerter) Send(al alerter.Alert)               { a.sent <- al }
func (a *recordingAlerter) Normal(string, string, ...string)    {}
func (a *recordingAlerter) Urgent(string, string, ...string)    {}
func (a *recordingAlerter) Emergency(string, string, ...string) {}
func (a *recordingAlerter) Close(context.Context) error         { return nil }

// newServerOn builds a server whose listener is ln.
func newServerOn(t *testing.T, ln net.Listener, opts ...Option) *MCPServer {
	t.Helper()
	srv, err := New(append(minimalOpts(newRegistryWith()), opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.netListen = func(string, string) (net.Listener, error) { return ln, nil }
	return srv
}

func TestStart_BindFailureIsReturned(t *testing.T) {
	srv, err := New(append(minimalOpts(newRegistryWith()), WithListen("127.0.0.1:1"))...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bindErr := errors.New("address already in use")
	srv.netListen = func(network, addr string) (net.Listener, error) {
		if network != "tcp" || addr != "127.0.0.1:1" {
			t.Errorf("listen(%q, %q), want tcp on the configured address", network, addr)
		}
		return nil, bindErr
	}
	err = srv.Start()
	if !errors.Is(err, bindErr) || !strings.Contains(err.Error(), "bind 127.0.0.1:1") {
		t.Fatalf("Start() = %v, want the bind error naming the address", err)
	}
}

func TestStart_FailureInsideStartupWindowIsReturned(t *testing.T) {
	ln := newStubListener(nil)
	acceptErr := errors.New("accept failed")
	ln.fail <- acceptErr
	handed := make(chan error, 1)
	srv := newServerOn(t, ln, WithOnServeError(func(err error) { handed <- err }))

	err := srv.Start()
	if !errors.Is(err, acceptErr) || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("Start() = %v, want the serve error", err)
	}
	select {
	case err := <-handed:
		t.Fatalf("onServeError got %v for a failure Start returned", err)
	default:
	}
	shutdown(t, srv)
}

func TestStart_ServeErrorAfterStartupGoesToOnServeError(t *testing.T) {
	ln := newStubListener(nil)
	handed := make(chan error, 1)
	al := &recordingAlerter{sent: make(chan alerter.Alert, 1)}
	srv := newServerOn(t, ln, WithListen("127.0.0.1:5999"), WithAlerter(al),
		WithOnServeError(func(err error) { handed <- err }))

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	acceptErr := errors.New("listener died")
	ln.fail <- acceptErr

	err := <-handed
	if !errors.Is(err, acceptErr) || !strings.HasPrefix(err.Error(), "127.0.0.1:5999: ") {
		t.Fatalf("onServeError got %v, want the error prefixed with the listen address", err)
	}
	select {
	case a := <-al.sent:
		t.Fatalf("alerter got %+v although onServeError is installed", a)
	default:
	}
	shutdown(t, srv)
}

func TestStart_ServeErrorAfterStartupAlerts(t *testing.T) {
	ln := newStubListener(nil)
	al := &recordingAlerter{sent: make(chan alerter.Alert, 1)}
	srv := newServerOn(t, ln, WithListen("127.0.0.1:5998"), WithAlerter(al))

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ln.fail <- errors.New("listener died")

	a := <-al.sent
	if a.Title != "MCP host server stopped" || a.EventID != "mcpserver" ||
		a.Details != "listener died" || !strings.HasPrefix(a.Description, "127.0.0.1:5998: ") {
		t.Fatalf("alert = %+v", a)
	}
	shutdown(t, srv)
}

func TestStartShutdown_ServesBothEndpoints(t *testing.T) {
	var ln net.Listener
	srv, err := New(append(minimalOpts(newRegistryWith(&mockTool{name: "read_file", params: map[string]any{}, result: toolkit.NewResult("ok")})),
		WithAllowlist([]string{"*"}))...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.netListen = func(network, _ string) (net.Listener, error) {
		l, lerr := net.Listen(network, "127.0.0.1:0")
		ln = l
		return l, lerr
	}
	if err = srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + ln.Addr().String()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}

	// post lists the tools on path and returns the status and body.
	post := func(path string) (int, string, error) {
		req, rerr := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, rerr := http.DefaultClient.Do(req)
		if rerr != nil {
			return 0, "", rerr
		}
		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Errorf("close body: %v", cerr)
			}
		}()
		raw, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			t.Fatalf("read body: %v", rerr)
		}
		return resp.StatusCode, string(raw), nil
	}

	code, out, err := post(InternalEndpointPath)
	if err != nil {
		t.Fatalf("POST /internal: %v", err)
	}
	if code != http.StatusOK || !strings.Contains(out, "read_file") {
		t.Fatalf("/internal tools/list = %d %s, want the tool listed", code, out)
	}
	code, _, err = post(DefaultEndpointPath)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("/mcp without a bearer = %d, want 401", code)
	}

	shutdown(t, srv)
	if _, _, err = post(InternalEndpointPath); err == nil {
		t.Fatal("server still answers after Shutdown")
	}
}

// shutdown shuts srv down, failing the test on an error.
func shutdown(t *testing.T, srv *MCPServer) {
	t.Helper()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestShutdown_ListenerCloseErrorIsLoggedNotReturned(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()
	ln := newStubListener(errors.New("close failed"))
	srv := newServerOn(t, ln)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "Failed to close MCP HTTP server") {
		t.Fatalf("log lacks the close failure:\n%s", buf.String())
	}
}

// A listener that fails inside the startup window is a start failure: Start
// returns it and no operator alert is sent, since the caller reports it.
func TestStart_FailureInsideStartupWindowDoesNotAlert(t *testing.T) {
	ln := newStubListener(nil)
	acceptErr := errors.New("accept failed")
	ln.fail <- acceptErr
	al := &recordingAlerter{sent: make(chan alerter.Alert, 1)}
	srv := newServerOn(t, ln, WithAlerter(al))

	if err := srv.Start(); !errors.Is(err, acceptErr) {
		t.Fatalf("Start() = %v, want the serve error", err)
	}
	select {
	case a := <-al.sent:
		t.Fatalf("alerter got %+v for a failure Start returned", a)
	default:
	}
	shutdown(t, srv)
}
