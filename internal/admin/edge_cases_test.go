// Agency
// License: MIT

package admin

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failWriter fails every write after the first ok writes.
type failWriter struct{ ok int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.ok > 0 {
		w.ok--
		return len(p), nil
	}
	return 0, errors.New("write refused")
}

// failReader fails every read.
type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("read refused") }

// stubTerminal replaces the terminal checks: stdinTTY says whether stdin is a
// terminal, and tty is what opening /dev/tty yields (a terminal when ttyIsTerm).
func stubTerminal(t *testing.T, stdinTTY bool, tty func() (*os.File, error), ttyIsTerm bool) {
	t.Helper()
	origIs, origOpen := isTerminal, openTTY
	t.Cleanup(func() { isTerminal, openTTY = origIs, origOpen })
	stdinFD := int(os.Stdin.Fd())
	isTerminal = func(fd int) bool {
		if fd == stdinFD {
			return stdinTTY
		}
		return ttyIsTerm
	}
	openTTY = tty
}

func tempTTY(t *testing.T) func() (*os.File, error) {
	t.Helper()
	return func() (*os.File, error) {
		return os.OpenFile(filepath.Join(t.TempDir(), "tty"), os.O_RDWR|os.O_CREATE, 0o600)
	}
}

func TestOpenTerminal(t *testing.T) {
	t.Run("stdin is a terminal", func(t *testing.T) {
		stubTerminal(t, true, func() (*os.File, error) {
			t.Error("/dev/tty opened although stdin is a terminal")
			return nil, errors.New("unexpected")
		}, false)
		out := &strings.Builder{}
		term, err := OpenTerminal(out)
		if err != nil {
			t.Fatal(err)
		}
		if term.In != os.Stdin || term.Out != out || term.ReadPassword == nil {
			t.Errorf("terminal = %+v, want stdin with prompts on out", term)
		}
		if err := term.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if _, err := term.ReadPassword(); err == nil {
			t.Error("ReadPassword on a stdin that is no terminal succeeded")
		}
	})
	t.Run("no /dev/tty", func(t *testing.T) {
		stubTerminal(t, false, func() (*os.File, error) { return nil, os.ErrNotExist }, false)
		_, err := OpenTerminal(io.Discard)
		if !errors.Is(err, ErrNoTerminal) || !strings.Contains(err.Error(), "/dev/tty") {
			t.Fatalf("err = %v, want ErrNoTerminal naming /dev/tty", err)
		}
	})
	t.Run("/dev/tty is not a terminal", func(t *testing.T) {
		var opened *os.File
		open := tempTTY(t)
		stubTerminal(t, false, func() (*os.File, error) {
			f, err := open()
			opened = f
			return f, err
		}, false)
		_, err := OpenTerminal(io.Discard)
		if !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("err = %v, want ErrNoTerminal", err)
		}
		if _, werr := opened.WriteString("x"); werr == nil {
			t.Error("the non-terminal /dev/tty was left open")
		}
	})
	t.Run("/dev/tty", func(t *testing.T) {
		stubTerminal(t, false, tempTTY(t), true)
		term, err := OpenTerminal(io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f, ok := term.In.(*os.File)
		if w, wok := term.Out.(*os.File); !ok || !wok || w != f || term.ReadPassword == nil {
			t.Fatalf("terminal = %+v, want /dev/tty for both directions", term)
		}
		if _, err := term.ReadPassword(); err == nil {
			t.Error("ReadPassword on a file that is no terminal succeeded")
		}
		if err := term.Close(); err != nil {
			t.Fatal(err)
		}
		if _, werr := f.WriteString("x"); werr == nil {
			t.Error("Close did not close /dev/tty")
		}
	})
}

func TestParseHash_Malformed(t *testing.T) {
	for _, h := range []string{
		"$argon2id$19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5",       // no v=
		"$argon2id$v=19$m=65536,t3,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5",      // no '='
		"$argon2id$v=19$m=65536,t=x,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5",     // not a number
		"$argon2id$v=19$m=65536,t=3,p=1,q=2$c2FsdHNhbHRzYWx0c2FsdA$a2V5", // unknown parameter
	} {
		if err := ParseHash(h); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("ParseHash(%q) = %v, want ErrInvalidHash", h, err)
		}
	}
}

func TestLoad_ReadErrors(t *testing.T) {
	dir := t.TempDir()
	t.Run("path under a file", func(t *testing.T) {
		blocker := filepath.Join(dir, "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(filepath.Join(blocker, FileName), DefaultFileMode)
		if err == nil || errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "stat credentials file") {
			t.Fatalf("err = %v, want a stat error", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		p := filepath.Join(dir, "asdir")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, DefaultFileMode); err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		p := filepath.Join(dir, "unreadable.json")
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o200); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, DefaultFileMode); err == nil || !strings.Contains(err.Error(), "read credentials file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not JSON", func(t *testing.T) {
		p := filepath.Join(dir, "garbled.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, DefaultFileMode); err == nil || !strings.Contains(err.Error(), "parse credentials file") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWrite_Errors(t *testing.T) {
	dir := t.TempDir()
	t.Run("parent is a file", func(t *testing.T) {
		blocker := filepath.Join(dir, "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := Write(filepath.Join(blocker, "sub", FileName), "alice", "a long enough password", DefaultFileMode)
		if err == nil || !strings.Contains(err.Error(), "create ") {
			t.Fatalf("err = %v, want a create error", err)
		}
	})
	t.Run("directory not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(ro, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		err := Write(Path(ro), "alice", "a long enough password", DefaultFileMode)
		if err == nil || !strings.Contains(err.Error(), "create temp file") {
			t.Fatalf("err = %v, want a temp file error", err)
		}
	})
	t.Run("target is a directory", func(t *testing.T) {
		home := filepath.Join(dir, "home")
		if err := os.MkdirAll(filepath.Join(Path(home), "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := Write(Path(home), "alice", "a long enough password", DefaultFileMode)
		if err == nil || !strings.Contains(err.Error(), "replace ") {
			t.Fatalf("err = %v, want a replace error", err)
		}
		entries, rerr := os.ReadDir(home)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(entries) != 1 {
			t.Errorf("temp file left behind after a failed replace: %v", entries)
		}
	})
}

func TestPromptUsername_IOErrors(t *testing.T) {
	t.Run("prompt write fails", func(t *testing.T) {
		_, err := promptUsername(&Terminal{In: strings.NewReader("alice\n"), Out: &failWriter{}})
		if err == nil {
			t.Fatal("write error ignored")
		}
	})
	t.Run("read fails", func(t *testing.T) {
		_, err := promptUsername(&Terminal{In: failReader{}, Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "read username: read refused") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("error message write fails", func(t *testing.T) {
		_, err := promptUsername(&Terminal{In: strings.NewReader("has space\nalice\n"), Out: &failWriter{ok: 1}})
		if err == nil || err.Error() != "write refused" {
			t.Fatalf("err = %v, want the write error", err)
		}
	})
	t.Run("gives up after three invalid names", func(t *testing.T) {
		_, err := promptUsername(&Terminal{In: strings.NewReader("a b\nc d\ne f\nalice\n"), Out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "whitespace") {
			t.Fatalf("err = %v, want the last validation error", err)
		}
	})
}

func secrets(s ...string) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(s) == 0 {
			return nil, io.EOF
		}
		v := s[0]
		s = s[1:]
		return []byte(v), nil
	}
}

func TestPromptPassword_IOErrors(t *testing.T) {
	const good = "a long enough password"
	tests := []struct {
		name string
		term *Terminal
		want string
	}{
		{"confirm read fails", &Terminal{Out: io.Discard, ReadPassword: secrets(good)}, "read password"},
		{"error message write fails", &Terminal{Out: &failWriter{ok: 2}, ReadPassword: secrets("short")}, "write refused"},
		{"prompt write fails", &Terminal{Out: &failWriter{}, ReadPassword: secrets(good)}, "write refused"},
		{"newline write fails", &Terminal{Out: &failWriter{ok: 1}, ReadPassword: secrets(good)}, "write refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := promptPassword(tc.term)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEnsureAccount_Defaults(t *testing.T) {
	if _, _, err := EnsureAccount(EnsureOptions{}); err == nil || !strings.Contains(err.Error(), "AGENCY_HOME is empty") {
		t.Fatalf("EnsureAccount without a home = %v", err)
	}
	// Without Getenv the process environment supplies the account.
	t.Setenv(EnvUser, "alice")
	t.Setenv(EnvPassword, "a long enough password")
	home := t.TempDir()
	user, created, err := EnsureAccount(EnsureOptions{Home: home})
	if err != nil || user != "alice" || !created {
		t.Fatalf("EnsureAccount = %q, %v, %v", user, created, err)
	}
}

// TestEnsureAccount_DefaultTerminal: without OpenTerminal the real terminal
// check runs (stubbed here to find none).
func TestEnsureAccount_DefaultTerminal(t *testing.T) {
	stubTerminal(t, false, func() (*os.File, error) { return nil, os.ErrNotExist }, false)
	_, _, err := EnsureAccount(EnsureOptions{Home: t.TempDir(), Getenv: noEnv})
	if !errors.Is(err, ErrNoAccountNoTerminal) {
		t.Fatalf("err = %v, want ErrNoAccountNoTerminal", err)
	}
}

func TestEnsureAccount_WriteFails(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(Path(home), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory at the credentials path does not load either, so the
	// failure is reported as an unusable account, never overwritten.
	_, _, err := EnsureAccount(EnsureOptions{Home: home, Getenv: env("alice", "a long enough password")})
	if err == nil || !strings.Contains(err.Error(), "existing admin account unusable") {
		t.Fatalf("err = %v", err)
	}
	if os.Geteuid() == 0 {
		return // root writes anywhere
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if ierr := os.Mkdir(ro, 0o500); ierr != nil {
		t.Fatal(ierr)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(ro, 0o700); cerr != nil {
			t.Error(cerr)
		}
	})
	_, created, err := EnsureAccount(EnsureOptions{Home: ro, Getenv: env("alice", "a long enough password")})
	if err == nil || created || !strings.Contains(err.Error(), "create temp file") {
		t.Fatalf("EnsureAccount in a read-only home = %v (created %v)", err, created)
	}
}

// TestEnsureAccount_ChownAndRemoveFail: when the chown fails and the file
// cannot be removed either, both are reported.
func TestEnsureAccount_ChownAndRemoveFail(t *testing.T) {
	home := t.TempDir()
	_, _, err := EnsureAccount(EnsureOptions{
		Home:   home,
		Getenv: env("alice", "a long enough password"),
		Chown: func(p string) error {
			// Replace the file with a non-empty directory so removing it fails.
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(p, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			return errors.New("chown refused")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "chown refused") || !strings.Contains(err.Error(), "removing "+Path(home)+" failed") {
		t.Fatalf("err = %v, want both failures", err)
	}
}

func TestPromptOnTerminal_Errors(t *testing.T) {
	t.Run("open fails", func(t *testing.T) {
		boom := errors.New("tty busy")
		_, _, err := promptOnTerminal(func(io.Writer) (*Terminal, error) { return nil, boom }, io.Discard, "p")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the open error unchanged", err)
		}
	})
	t.Run("intro write fails", func(t *testing.T) {
		closed := false
		_, _, err := promptOnTerminal(func(io.Writer) (*Terminal, error) {
			return &Terminal{Out: &failWriter{}, Close: func() error { closed = true; return nil }}, nil
		}, io.Discard, "p")
		if err == nil || err.Error() != "write refused" || !closed {
			t.Fatalf("err = %v, closed %v", err, closed)
		}
	})
	t.Run("close error reported", func(t *testing.T) {
		_, _, err := promptOnTerminal(func(io.Writer) (*Terminal, error) {
			return &Terminal{
				In: strings.NewReader("alice\n"), Out: io.Discard,
				ReadPassword: secrets("a long enough password", "a long enough password"),
				Close:        func() error { return errors.New("eio") },
			}, nil
		}, io.Discard, "p")
		if err == nil || !strings.Contains(err.Error(), "close terminal: eio") {
			t.Fatalf("err = %v", err)
		}
	})
}

// stubCreateTemp makes Write's temp file the one mk returns for the path the
// real CreateTemp chose.
func stubCreateTemp(t *testing.T, mk func(t *testing.T, name string) *os.File) {
	t.Helper()
	orig := createTemp
	t.Cleanup(func() { createTemp = orig })
	createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := orig(dir, pattern)
		if err != nil {
			return nil, err
		}
		name := f.Name()
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return mk(t, name), nil
	}
}

// TestWrite_TempFileFailures: a temp file that cannot be set to the mode or
// written is removed, and the existing account is left as it was.
func TestWrite_TempFileFailures(t *testing.T) {
	tests := []struct {
		name string
		mk   func(t *testing.T, name string) *os.File
		want string
	}{
		{"chmod fails", func(t *testing.T, name string) *os.File {
			t.Helper()
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			if cerr := f.Close(); cerr != nil { // a closed file refuses Chmod
				t.Error(cerr)
			}
			return f
		}, "chmod temp file"},
		{"write fails", func(t *testing.T, name string) *os.File {
			t.Helper()
			f, err := os.Open(name) // read-only
			if err != nil {
				t.Fatal(err)
			}
			return f
		}, "write temp file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := Write(Path(home), "alice", "a long enough password", DefaultFileMode); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(Path(home))
			if err != nil {
				t.Fatal(err)
			}
			stubCreateTemp(t, tc.mk)
			err = Write(Path(home), "bob", "another long password", DefaultFileMode)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Write = %v, want %q", err, tc.want)
			}
			after, rerr := os.ReadFile(Path(home))
			if rerr != nil || string(after) != string(before) {
				t.Errorf("existing account changed by a failed write (%v)", rerr)
			}
			entries, rerr := os.ReadDir(home)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 1 {
				t.Errorf("temp file left behind: %v", entries)
			}
		})
	}
}

// TestWrite_TempFileGoneStillFails: a temp file that has vanished by the
// time it is cleaned up only adds a warning; the write still fails.
func TestWrite_TempFileGoneStillFails(t *testing.T) {
	home := t.TempDir()
	stubCreateTemp(t, func(t *testing.T, name string) *os.File {
		t.Helper()
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		return f
	})
	if err := Write(Path(home), "alice", "a long enough password", DefaultFileMode); err == nil {
		t.Fatal("Write to a read-only temp file succeeded")
	}
	if _, err := os.Stat(Path(home)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credentials file created: %v", err)
	}
}
