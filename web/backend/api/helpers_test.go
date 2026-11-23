// ClawEh
// License: MIT

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHTTPErrorMessage(t *testing.T) {
	err := notFound("Index %d out of range", 7)
	if err.Error() != "Index 7 out of range" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

// An unencodable value is logged; the status already sent stands.
func TestEncodeJSON_Unencodable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"c": make(chan int)})
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q, want 200 and nothing written", rec.Code, rec.Body.String())
	}
}

func TestGatewayProbeHost(t *testing.T) {
	for in, want := range map[string]string{"": "127.0.0.1", "0.0.0.0": "127.0.0.1", "192.168.1.20": "192.168.1.20", "::1": "::1"} {
		if got := gatewayProbeHost(in); got != want {
			t.Errorf("gatewayProbeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTopLevelJSON_Nil(t *testing.T) {
	if topLevelJSON(nil) != nil {
		t.Fatal("nil config must give nil")
	}
}

func TestUniqueName(t *testing.T) {
	taken := map[string]struct{}{"Claude": {}, "Claude 2": {}}
	if got := uniqueName("Claude", taken); got != "Claude 3" {
		t.Fatalf("uniqueName = %q, want Claude 3", got)
	}
	if got := uniqueName("Codex", taken); got != "Codex" {
		t.Fatalf("uniqueName = %q, want Codex", got)
	}
}

// An explicit path that exists but is not executable still resolves (a
// permissions problem, not a missing binary); a missing one, or an unknown
// protocol with no command, does not.
func TestResolveCLIBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits")
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "claude")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := resolveCLIBinary("claude-cli", plain); !ok || got != plain {
		t.Fatalf("non-executable path = %q %v, want it resolved", got, ok)
	}
	if got, ok := resolveCLIBinary("claude-cli", filepath.Join(dir, "missing")); ok || got != "" {
		t.Fatalf("missing path = %q %v", got, ok)
	}
	if got, ok := resolveCLIBinary("no-such-cli", ""); ok || got != "" {
		t.Fatalf("unknown protocol = %q %v", got, ok)
	}
	if got, ok := resolveCLIBinary("claude-cli", dir); ok || got != "" {
		t.Fatalf("a directory = %q %v, want not resolved", got, ok)
	}
}

// cliVersion returns the first line of "<bin> --version", or "" when the
// command fails.
func TestCLIVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\necho \"  tool 1.2.3  \"\necho second\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(dir, "fail")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := cliVersion(context.Background(), ok); got != "tool 1.2.3" {
		t.Fatalf("cliVersion = %q, want tool 1.2.3", got)
	}
	if got := cliVersion(context.Background(), fail); got != "" {
		t.Fatalf("failing command = %q, want empty", got)
	}
}

// On Linux the OS name is the os-release PRETTY_NAME.
func TestOSName(t *testing.T) {
	got := osName()
	switch runtime.GOOS {
	case "linux":
		p := filepath.Join(t.TempDir(), "os-release")
		if err := os.WriteFile(p, []byte("NAME=x\nPRETTY_NAME=\"Ubuntu 26.04 LTS\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		orig := osReleasePath
		osReleasePath = p
		t.Cleanup(func() { osReleasePath = orig })
		if got = osName(); got != "Ubuntu 26.04 LTS" {
			t.Fatalf("osName = %q", got)
		}
	case "darwin":
		if got != "macOS" {
			t.Fatalf("osName = %q", got)
		}
	case "windows":
		if got != "Windows" {
			t.Fatalf("osName = %q", got)
		}
	}
}
