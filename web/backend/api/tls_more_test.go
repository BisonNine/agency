// ClawEh
// License: MIT

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/tlscert"
)

// With a listener running, GET /api/tls reports the certificate it serves;
// with none, the one the saved files hold, or why it cannot be read; a blank
// HTTP host reads as 127.0.0.1.
func TestGetTLS_CertificateSources(t *testing.T) {
	h, mux, _ := tlsTestHandler(t)
	certPath, keyPath := selfSignedPair(t, time.Now())
	setTLS := func(cert, key string) {
		t.Helper()
		if err := h.updateConfig(nil, func(c *config.Config) error {
			c.Gateway.Host = ""
			c.Gateway.TLS.CertFile, c.Gateway.TLS.KeyFile = cert, key
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	setTLS(certPath, keyPath)
	var got tlsStatusResponse
	if code := serveJSON(t, mux, http.MethodGet, "/api/tls", "", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.HTTPHost != "127.0.0.1" {
		t.Errorf("http_host = %q, want 127.0.0.1", got.HTTPHost)
	}
	if got.CertFile != certPath || got.Certificate == nil || !got.Certificate.Present || got.Certificate.Fingerprint == "" {
		t.Fatalf("saved-file certificate = %+v (cert_file %q)", got.Certificate, got.CertFile)
	}
	fileFingerprint := got.Certificate.Fingerprint

	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	setTLS(bad, keyPath)
	got = tlsStatusResponse{}
	if code := serveJSON(t, mux, http.MethodGet, "/api/tls", "", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.Certificate == nil || got.Certificate.Present || got.Certificate.Error == "" || got.Certificate.Names == nil {
		t.Fatalf("unreadable certificate = %+v, want an error and no certificate", got.Certificate)
	}

	m, err := tlscert.Load(tlscert.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.SetTLSManager(m)
	got = tlsStatusResponse{}
	if code := serveJSON(t, mux, http.MethodGet, "/api/tls", "", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.Certificate == nil || got.Certificate.Fingerprint != m.Info().Fingerprint || got.Certificate.Fingerprint == fileFingerprint {
		t.Fatalf("running certificate = %+v, want the manager's %s", got.Certificate, m.Info().Fingerprint)
	}
}

// Regenerating with an unreadable config is a 500 naming the failure, and
// the certificate is left as it was.
func TestRegenerateTLS_BrokenConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAW_HOME", filepath.Join(dir, ".claw"))
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	m, err := tlscert.Load(tlscert.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	before := m.Info().Fingerprint
	h.SetTLSManager(m)
	var e struct {
		Error string `json:"error"`
	}
	if code := serveJSON(t, mux, http.MethodPost, "/api/tls/regenerate", "", &e); code != http.StatusInternalServerError ||
		!strings.HasPrefix(e.Error, "failed to load config: ") {
		t.Fatalf("status %d error %q", code, e.Error)
	}
	if m.Info().Fingerprint != before {
		t.Fatal("certificate changed on a failed request")
	}
}
