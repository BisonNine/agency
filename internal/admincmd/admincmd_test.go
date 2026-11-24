// Agency
// License: MIT

package admincmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"go.bison9.com/agency/internal/admin"
	"go.bison9.com/agency/internal/install"
)

func TestResolveHome_EnvWins(t *testing.T) {
	stubDetect(t, &install.ExistingInstall{AgencyHome: "/opt/agency"})
	t.Setenv("AGENCY_HOME", "/tmp/claw-test-home")
	home, _ := ResolveHome()
	if home != "/tmp/claw-test-home" {
		t.Fatalf("ResolveHome() = %q", home)
	}
}

func TestRun_RequiresTerminal(t *testing.T) {
	// Neither stdin nor /dev/tty is a terminal: the password must never be
	// read from a pipe or a redirect, so the command refuses.
	orig := openTerminal
	t.Cleanup(func() { openTerminal = orig })
	openTerminal = func(io.Writer) (*admin.Terminal, error) {
		return nil, fmt.Errorf("%w: test", admin.ErrNoTerminal)
	}
	err := run(io.Discard, "alice")
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("run() without a terminal = %v", err)
	}
}

func TestRun_WritesAccountFromTerminal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENCY_HOME", home)
	orig := openTerminal
	t.Cleanup(func() { openTerminal = orig })
	var prompts bytes.Buffer
	secrets := []string{"a long enough password", "a long enough password"}
	closed := false
	openTerminal = func(io.Writer) (*admin.Terminal, error) {
		return &admin.Terminal{
			In:  strings.NewReader("bob\n"),
			Out: &prompts,
			ReadPassword: func() ([]byte, error) {
				if len(secrets) == 0 {
					return nil, errors.New("no more input")
				}
				s := secrets[0]
				secrets = secrets[1:]
				return []byte(s), nil
			},
			Close: func() error { closed = true; return nil },
		}, nil
	}

	var out bytes.Buffer
	if err := run(&out, ""); err != nil {
		t.Fatalf("run() = %v", err)
	}
	creds, err := admin.Load(admin.Path(home), admin.DefaultFileMode)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if creds.Username != "bob" || !creds.Verify("bob", "a long enough password") {
		t.Fatalf("stored account does not match: %+v", creds)
	}
	if !closed {
		t.Error("terminal was not closed")
	}
	if !strings.Contains(prompts.String(), "Username: ") || !strings.Contains(out.String(), `Admin account "bob" written`) {
		t.Errorf("prompts %q, output %q", prompts.String(), out.String())
	}
}
