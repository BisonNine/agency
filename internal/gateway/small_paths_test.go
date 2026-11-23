// ClawEh
// License: MIT

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a3tai/openclaw-go/protocol"

	"go.bison9.com/agency/forum"
	"go.bison9.com/agency/internal/tlscert"
	"go.bison9.com/agency/utils"
)

func TestRestartRequestedError(t *testing.T) {
	err := &restartRequestedError{}
	if !strings.Contains(err.Error(), "restart requested through the API") || exitCode(err) != exitCodeServiceDied {
		t.Errorf("restart error = %q (exit %d)", err, exitCode(err))
	}
	if exitCode(errors.New("bad config")) != 1 {
		t.Error("a startup error does not exit 1")
	}
}

// TestFatalService_UnknownService: a service without its own alert still
// raises one that names it.
func TestFatalService_UnknownService(t *testing.T) {
	f, rec, _ := newTestFatal(t)
	f.fatalService("telemetry", errors.New("gone"))
	got := rec.Alerts()
	if len(got) != 1 || got[0].Title != "Core service stopped" || !strings.Contains(got[0].Description, "telemetry died") {
		t.Fatalf("alerts = %+v", got)
	}
	if died := <-f.failed; died.Service != "telemetry" {
		t.Errorf("failure for %q", died.Service)
	}
}

// TestRecoverForums_NoScopes: with no agent to recover, recovery ends at once
// after the loop starts; closing no service is a no-op.
func TestRecoverForums_NoScopes(t *testing.T) {
	started := make(chan struct{})
	close(started)
	done := make(chan struct{})
	recoverForums(context.Background(), nil, func() []forum.Scope { return nil }, started, func() { close(done) })
	<-done
	closeForums(nil)
}

func TestAcquireLock_Errors(t *testing.T) {
	t.Run("data dir is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLock(f); err == nil || !strings.Contains(err.Error(), "cannot create directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("lock path is a folder", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "internal", lockFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireLock(home); err == nil || !strings.Contains(err.Error(), "cannot open lock file") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestReleaseLock_Failures: a lock whose file is already closed and removed
// is released with warnings, not a panic.
func TestReleaseLock_Failures(t *testing.T) {
	home := t.TempDir()
	f, err := acquireLock(home)
	if err != nil {
		t.Fatal(err)
	}
	utils.CloseQuietly(f)
	if err := os.Remove(f.Name()); err != nil {
		t.Fatal(err)
	}
	out := logLines(t, func() { releaseLock(f) })
	for _, want := range []string{"failed to release lock file", "failed to close lock file", "failed to remove lock file"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestDefaultDeviceWSURL(t *testing.T) {
	for _, tc := range []struct {
		host   string
		port   int
		useTLS bool
		want   string
	}{
		{"", 0, false, "ws://127.0.0.1:18791/"},
		{"0.0.0.0", 19000, true, "wss://127.0.0.1:19000/"},
		{"::1", 19001, false, "ws://[::1]:19001/"},
	} {
		if got := defaultDeviceWSURL(tc.host, tc.port, tc.useTLS); got != tc.want {
			t.Errorf("defaultDeviceWSURL(%q, %d, %v) = %q, want %q", tc.host, tc.port, tc.useTLS, got, tc.want)
		}
	}
	if isPairingError(nil) {
		t.Error("nil is a pairing error")
	}
}

// TestPinnedGatewayTLS_Refusals: no peer certificate, or no local certificate
// to compare with, refuses the connection.
func TestPinnedGatewayTLS_Refusals(t *testing.T) {
	cfg := pinnedGatewayTLS(tlscert.Options{DataDir: t.TempDir()})
	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil || !strings.Contains(err.Error(), "presented no certificate") {
		t.Errorf("no peer certificate: %v", err)
	}
	other := t.TempDir()
	m, err := tlscert.Load(tlscert.Options{DataDir: other})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(m.Info().CertFile, m.Info().KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil || !strings.Contains(err.Error(), "read the TLS certificate") {
		t.Errorf("no local certificate: %v", err)
	}
}

// TestBridgeLocalStoreErrors: a data directory the bridge cannot use is
// reported, both for its identity and for self-approval.
func TestBridgeLocalStoreErrors(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "internal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connectBridge(context.Background(), home, "ws://127.0.0.1:1/", "", nil, true, func(protocol.Event) {}); err == nil || !strings.Contains(err.Error(), "create identity dir") {
		t.Errorf("connectBridge = %v", err)
	}
	if _, err := autoApproveLocalDevice(context.Background(), home, "dev"); err == nil || !strings.Contains(err.Error(), "open device store") {
		t.Errorf("autoApproveLocalDevice = %v", err)
	}
}
