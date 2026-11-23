// ClawEh
// License: MIT

package admincmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/internal/admin"
	"go.bison9.com/agency/internal/install"
)

// stubDetect makes ResolveHome see inst as the installed service.
func stubDetect(t *testing.T, inst *install.ExistingInstall) {
	t.Helper()
	orig := detectInstall
	t.Cleanup(func() { detectInstall = orig })
	detectInstall = func(string) *install.ExistingInstall { return inst }
}

// stubEUID makes the command believe it runs with effective uid uid.
func stubEUID(t *testing.T, uid int) {
	t.Helper()
	orig := geteuid
	t.Cleanup(func() { geteuid = orig })
	geteuid = func() int { return uid }
}

// fakeTerminal answers the username prompt with username and each password
// prompt with the next of passwords; closeErr is what Close returns.
func fakeTerminal(t *testing.T, username string, passwords []string, closeErr error) {
	t.Helper()
	orig := openTerminal
	t.Cleanup(func() { openTerminal = orig })
	openTerminal = func(io.Writer) (*admin.Terminal, error) {
		return &admin.Terminal{
			In:  strings.NewReader(username + "\n"),
			Out: io.Discard,
			ReadPassword: func() ([]byte, error) {
				if len(passwords) == 0 {
					return nil, errors.New("no more input")
				}
				p := passwords[0]
				passwords = passwords[1:]
				return []byte(p), nil
			},
			Close: func() error { return closeErr },
		}, nil
	}
}

const goodPassword = "a long enough password"

func TestResolveHome_InstalledServiceHome(t *testing.T) {
	stubDetect(t, &install.ExistingInstall{ClawHome: "/srv/claw", User: "alice"})
	t.Setenv("CLAW_HOME", "")
	home, inst := ResolveHome()
	if home != "/srv/claw" || inst == nil || inst.User != "alice" {
		t.Fatalf("ResolveHome() = %q, %+v; want the installed service's home", home, inst)
	}
}

func TestResolveHome_DefaultUnderUserHome(t *testing.T) {
	for name, inst := range map[string]*install.ExistingInstall{
		"no install":           nil,
		"install without home": {BinaryPath: "/usr/local/bin/claw"},
	} {
		t.Run(name, func(t *testing.T) {
			stubDetect(t, inst)
			userHome := t.TempDir()
			t.Setenv("HOME", userHome)
			t.Setenv("CLAW_HOME", "")
			home, _ := ResolveHome()
			if want := filepath.Join(userHome, ".claw"); home != want {
				t.Fatalf("ResolveHome() = %q, want %q", home, want)
			}
		})
	}
}

// TestResolveHome_NoUserHome: without a home directory the fallback is a
// relative .claw rather than a path under the filesystem root.
func TestResolveHome_NoUserHome(t *testing.T) {
	var seen string
	orig := detectInstall
	t.Cleanup(func() { detectInstall = orig })
	detectInstall = func(h string) *install.ExistingInstall { seen = h; return nil }
	t.Setenv("HOME", "")
	t.Setenv("CLAW_HOME", "")
	home, _ := ResolveHome()
	if home != ".claw" || seen != "" {
		t.Fatalf("ResolveHome() = %q (detection given %q), want .claw and no home", home, seen)
	}
}

func TestSecretsFileMode(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		mode, err := SecretsFileMode(t.TempDir())
		if err != nil || mode != 0o600 {
			t.Fatalf("SecretsFileMode = %04o, %v; want the default 0600", mode, err)
		}
	})
	t.Run("configured", func(t *testing.T) {
		home := t.TempDir()
		body := `{"security":{"permissions":{"secrets":{"dir_mode":"0750","file_mode":"0640"}}}}`
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mode, err := SecretsFileMode(home)
		if err != nil || mode != 0o640 {
			t.Fatalf("SecretsFileMode = %04o, %v; want 0640", mode, err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, "config.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := SecretsFileMode(home); err == nil || !strings.Contains(err.Error(), "read security.permissions") {
			t.Fatalf("SecretsFileMode = %v, want a read error", err)
		}
	})
}

func TestRun_TerminalOpenError(t *testing.T) {
	orig := openTerminal
	t.Cleanup(func() { openTerminal = orig })
	boom := errors.New("tty busy")
	openTerminal = func(io.Writer) (*admin.Terminal, error) { return nil, boom }
	if err := run(io.Discard, "alice"); !errors.Is(err, boom) {
		t.Fatalf("run() = %v, want the terminal error unchanged", err)
	}
}

func TestRun_Errors(t *testing.T) {
	stubDetect(t, nil)
	t.Run("password mismatch", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAW_HOME", home)
		fakeTerminal(t, "", []string{goodPassword}, nil)
		if err := run(io.Discard, "alice"); err == nil {
			t.Fatal("run() succeeded without a confirmed password")
		}
		if _, err := os.Stat(admin.Path(home)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("credentials written after a failed prompt: %v", err)
		}
	})
	t.Run("unreadable config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAW_HOME", home)
		if err := os.Mkdir(filepath.Join(home, "config.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		fakeTerminal(t, "", []string{goodPassword, goodPassword}, nil)
		if err := run(io.Discard, "alice"); err == nil || !strings.Contains(err.Error(), "read security.permissions") {
			t.Fatalf("run() = %v, want the config error", err)
		}
	})
	t.Run("credentials path is a directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(admin.Path(home), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAW_HOME", home)
		fakeTerminal(t, "", []string{goodPassword, goodPassword}, nil)
		if err := run(io.Discard, "alice"); err == nil {
			t.Fatal("run() wrote credentials under a file")
		}
	})
	t.Run("close error reported", func(t *testing.T) {
		t.Setenv("CLAW_HOME", t.TempDir())
		fakeTerminal(t, "", []string{goodPassword, goodPassword}, errors.New("eio"))
		if err := run(io.Discard, "alice"); err == nil || !strings.Contains(err.Error(), "close terminal: eio") {
			t.Fatalf("run() = %v, want the close error", err)
		}
	})
	t.Run("close error does not mask an earlier one", func(t *testing.T) {
		t.Setenv("CLAW_HOME", t.TempDir())
		fakeTerminal(t, "", nil, errors.New("eio"))
		err := run(io.Discard, "alice")
		if err == nil || strings.Contains(err.Error(), "close terminal") {
			t.Fatalf("run() = %v, want the prompt error, not the close error", err)
		}
	})
}

// TestRun_RootChownsToServiceAccount: run as root against an installed
// service, the credentials file is handed to the service account; the uid is
// faked and the account is the current user, so the chown is allowed.
func TestRun_RootChownsToServiceAccount(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	stubDetect(t, &install.ExistingInstall{User: me.Username})
	stubEUID(t, 0)
	fakeTerminal(t, "", []string{goodPassword, goodPassword}, nil)
	var out bytes.Buffer
	if err := run(&out, "alice"); err != nil {
		t.Fatalf("run() = %v", err)
	}
	if !strings.Contains(out.String(), `Admin account "alice" written to `+admin.Path(home)) {
		t.Errorf("output = %q", out.String())
	}
}

func TestChownToService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("not root", func(t *testing.T) {
		stubEUID(t, 1000)
		if err := chownToService("/nonexistent", &install.ExistingInstall{User: "no-such-user-x"}); err != nil {
			t.Fatalf("non-root caller: %v", err)
		}
	})
	t.Run("root without install", func(t *testing.T) {
		stubEUID(t, 0)
		if err := chownToService("/nonexistent", nil); err != nil {
			t.Fatalf("no install: %v", err)
		}
	})
	for _, name := range []string{"", "root"} {
		t.Run("service account "+name, func(t *testing.T) {
			stubEUID(t, 0)
			if err := chownToService("/nonexistent", &install.ExistingInstall{User: name}); err != nil {
				t.Fatalf("service account %q: %v", name, err)
			}
		})
	}
	t.Run("unknown account", func(t *testing.T) {
		stubEUID(t, 0)
		err := chownToService(path, &install.ExistingInstall{User: "no-such-user-x"})
		if err == nil || !strings.Contains(err.Error(), `look up service user "no-such-user-x"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chown fails", func(t *testing.T) {
		me, err := user.Current()
		if err != nil {
			t.Skip("no current user")
		}
		stubEUID(t, 0)
		err = chownToService(filepath.Join(t.TempDir(), "missing"), &install.ExistingInstall{User: me.Username})
		if err == nil || !strings.Contains(err.Error(), "chown ") {
			t.Fatalf("err = %v, want a chown error", err)
		}
	})
}

func TestNewAdminCommand(t *testing.T) {
	stubDetect(t, nil)
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	fakeTerminal(t, "", []string{goodPassword, goodPassword}, nil)
	cmd := NewAdminCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"bob"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	creds, err := admin.Load(admin.Path(home), admin.DefaultFileMode)
	if err != nil || creds.Username != "bob" {
		t.Fatalf("Load = %+v, %v; want bob", creds, err)
	}

	fakeTerminal(t, "alice", []string{goodPassword, goodPassword}, nil)
	cmd = NewAdminCommand()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute without a username: %v", err)
	}
	if creds, err = admin.Load(admin.Path(home), admin.DefaultFileMode); err != nil || creds.Username != "alice" {
		t.Fatalf("Load = %+v, %v; want alice (prompted)", creds, err)
	}
}

// TestRun_ChownFailureReported: a service account that cannot be found is an
// error, not a credentials file the service cannot read.
func TestRun_ChownFailureReported(t *testing.T) {
	t.Setenv("CLAW_HOME", t.TempDir())
	stubDetect(t, &install.ExistingInstall{User: "no-such-user-x"})
	stubEUID(t, 0)
	fakeTerminal(t, "", []string{goodPassword, goodPassword}, nil)
	if err := run(io.Discard, "alice"); err == nil || !strings.Contains(err.Error(), "look up service user") {
		t.Fatalf("run() = %v, want the lookup error", err)
	}
}
