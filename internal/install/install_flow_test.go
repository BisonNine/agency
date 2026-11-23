package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/admin"
)

// installEnv is a machine for host.install: the running binary, the target
// account's home and the data directory all in temp dirs; service manager
// commands recorded; the account from the environment.
type installEnv struct {
	h        *host
	log      *cmdLog
	exe      string
	tu       *TargetUser
	clawHome string
	existing *ExistingInstall
	lookups  []string
	register []string // targetBin of each registration
}

// newInstallEnv builds the machine. With asRoot the account is the current
// one (so chowns to it succeed) and the service registration is recorded,
// never written.
func newInstallEnv(t *testing.T, goos string, asRoot bool) *installEnv {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	h, log := svcHost(t, goos)
	h.root = "" // install works on real temp paths; nothing below leaves them
	e := &installEnv{h: h, log: log}
	e.exe = filepath.Join(t.TempDir(), "claw-build")
	if err := os.WriteFile(e.exe, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.tu = &TargetUser{Username: me.Username, UID: me.Uid, GID: me.Gid, GroupName: "staff", HomeDir: t.TempDir(), IsRoot: asRoot}
	e.clawHome = filepath.Join(t.TempDir(), "data")
	t.Setenv(global.EnvVarHome, e.clawHome)
	t.Setenv("CLAW_HOME", e.clawHome)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(admin.EnvUser, "bob")
	t.Setenv(admin.EnvPassword, "bob's long password")

	h.executable = func() (string, error) { return e.exe, nil }
	h.targetUser = func(explicit string) (*TargetUser, error) {
		e.lookups = append(e.lookups, explicit)
		tu := *e.tu
		if explicit != "" {
			tu.Username = explicit
		}
		return &tu, nil
	}
	h.detect = func(string) *ExistingInstall { return e.existing }

	orig := registerService
	t.Cleanup(func() { registerService = orig })
	registerService = func(tu *TargetUser, targetBin, binDir, clawHome string) error {
		e.register = append(e.register, targetBin)
		if tu.IsRoot {
			return nil // a system service lives outside the temp dirs
		}
		return h.register(tu, targetBin, binDir, clawHome)
	}
	return e
}

func (e *installEnv) install(t *testing.T, host string, port int, cidrs, user, binDir string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = e.h.install(host, port, cidrs, user, binDir, true) })
	return out, err
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestInstall_UserModeLinux: a user install copies the binary to
// ~/.local/bin with the openclaw alias, puts it on PATH and exports
// CLAW_HOME in the shell rc, writes the bind and allowlist settings, creates
// the admin account and registers the user unit.
func TestInstall_UserModeLinux(t *testing.T) {
	e := newInstallEnv(t, "linux", false)
	out, err := e.install(t, "0.0.0.0", 18999, "192.168.1.0/24", "", "")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	binDir := filepath.Join(e.tu.HomeDir, ".local", "bin")
	target := filepath.Join(binDir, "claw")
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "new binary" {
		t.Fatalf("installed binary = %q, %v", b, rerr)
	}
	if link, lerr := os.Readlink(filepath.Join(binDir, openClawAlias)); lerr != nil || link != "claw" {
		t.Errorf("alias = %q, %v", link, lerr)
	}
	rc, rerr := os.ReadFile(filepath.Join(e.tu.HomeDir, ".bashrc"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{global.InstallRCMarker, `export PATH="` + binDir + `:$PATH"`, `export CLAW_HOME="` + e.clawHome + `"`} {
		if !strings.Contains(string(rc), want) {
			t.Errorf(".bashrc lacks %q:\n%s", want, rc)
		}
	}
	gw, ok := readJSON(t, filepath.Join(e.clawHome, "config.json"))["gateway"].(map[string]any)
	if !ok {
		t.Fatal("config has no gateway object")
	}
	if gw["host"] != "0.0.0.0" || gw["port"] != float64(18999) || fmt.Sprint(gw["allowed_cidrs"]) != "[192.168.1.0/24]" {
		t.Errorf("gateway = %v", gw)
	}
	creds, cerr := admin.Load(admin.Path(e.clawHome), admin.DefaultFileMode)
	if cerr != nil || creds.Username != "bob" {
		t.Fatalf("credentials = %+v, %v", creds, cerr)
	}
	if len(e.register) != 1 || e.register[0] != target {
		t.Errorf("registrations = %v", e.register)
	}
	if _, serr := os.Stat(userUnitPath(e.tu.HomeDir)); serr != nil {
		t.Errorf("user unit not written: %v", serr)
	}
	for _, want := range []string{
		"Mode:            User Mode (user space, no sudo required)",
		"Platform:        linux (systemd)",
		"Installed binary: " + target,
		"Installed alias:  " + filepath.Join(binDir, openClawAlias) + " -> claw",
		"Added " + binDir + " to PATH in ",
		"Created admin account \"bob\"",
		"Installed and running.",
		"WebUI on the network:",
		"Admin account: bob (",
		"Check status: systemctl --user status claw",
		"Uninstall:    " + target + " uninstall",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestInstall_SystemModeLinux: as root the install is a system service; the
// existing user service account is kept, and the hints use sudo.
func TestInstall_SystemModeLinux(t *testing.T) {
	e := newInstallEnv(t, "linux", true)
	e.existing = &ExistingInstall{ServiceType: "systemd-system", ServicePath: systemUnitPath, User: "carol", BinaryPath: "/nonexistent/claw"}
	binDir := t.TempDir()
	out, err := e.install(t, "", 0, "", "", binDir)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if len(e.lookups) != 2 || e.lookups[1] != "carol" {
		t.Errorf("target user lookups = %v, want the existing service's account kept", e.lookups)
	}
	for _, want := range []string{
		"Existing Service: " + systemUnitPath + " (systemd-system)",
		"Existing Binary:  /nonexistent/claw",
		"Mode:            System Mode (starts at system boot)",
		"Run As User:     carol",
		"Service File:    " + systemUnitPath,
		"Check status: systemctl status claw",
		"Uninstall:    sudo " + filepath.Join(binDir, "claw") + " uninstall",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(e.register) != 1 {
		t.Errorf("registrations = %v", e.register)
	}
}

func TestInstall_Darwin(t *testing.T) {
	for _, asRoot := range []bool{false, true} {
		e := newInstallEnv(t, "darwin", asRoot)
		out, err := e.install(t, "", 0, "", "", t.TempDir())
		if err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		wantFile, wantHint := userLaunchdPath(e.tu.HomeDir), "Check status: launchctl list | grep "+launchdLabel
		if asRoot {
			wantFile, wantHint = systemLaunchdPath, "Check status: sudo launchctl list | grep "+launchdLabel
		}
		for _, want := range []string{"Platform:        darwin (launchd)", "Service File:    " + wantFile, wantHint} {
			if !strings.Contains(out, want) {
				t.Errorf("root %v: output lacks %q:\n%s", asRoot, want, out)
			}
		}
	}
}

// TestInstall_Cancelled: declining the prompt installs nothing.
func TestInstall_Cancelled(t *testing.T) {
	e := newInstallEnv(t, "linux", false)
	binDir := t.TempDir()
	var err error
	var out string
	withStdin(t, "n\n", func() {
		out = captureStdout(t, func() { err = e.h.install("", 0, "", "", binDir, false) })
	})
	if err != nil || !strings.Contains(out, "Installation cancelled.") {
		t.Fatalf("install = %v\n%s", err, out)
	}
	if _, serr := os.Stat(filepath.Join(binDir, "claw")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("binary installed although cancelled: %v", serr)
	}
	if len(e.register) != 0 {
		t.Errorf("service registered although cancelled")
	}
}

func TestInstall_Errors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e *installEnv) (host, cidrs, binDir string)
		want  string
	}{
		{"unsupported system", func(_ *testing.T, e *installEnv) (string, string, string) {
			e.h.goos = "windows"
			return "", "", ""
		}, "only supported on Linux and macOS; this is windows"},
		{"no target user", func(_ *testing.T, e *installEnv) (string, string, string) {
			e.h.targetUser = func(string) (*TargetUser, error) { return nil, errors.New("must run as a regular user") }
			return "", "", ""
		}, "must run as a regular user"},
		{"other service kind installed", func(_ *testing.T, e *installEnv) (string, string, string) {
			e.existing = &ExistingInstall{ServiceType: "systemd-system", ServicePath: systemUnitPath}
			return "", "", ""
		}, "installed as a system service"},
		{"no executable", func(_ *testing.T, e *installEnv) (string, string, string) {
			e.h.executable = func() (string, error) { return "", errors.New("no /proc") }
			return "", "", ""
		}, "cannot locate running binary: no /proc"},
		{"bin dir blocked", func(t *testing.T, _ *installEnv) (string, string, string) {
			t.Helper()
			f := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return "", "", filepath.Join(f, "bin")
		}, "creating bin dir"},
		{"binary missing", func(t *testing.T, e *installEnv) (string, string, string) {
			t.Helper()
			e.exe = filepath.Join(t.TempDir(), "gone")
			return "", "", t.TempDir()
		}, "copying binary to"},
		{"config unreadable", func(t *testing.T, e *installEnv) (string, string, string) {
			t.Helper()
			if err := os.MkdirAll(e.clawHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.clawHome, "config.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			return "127.0.0.1", "", t.TempDir()
		}, "applying server settings"},
		{"network bind without allowlist", func(t *testing.T, _ *installEnv) (string, string, string) {
			t.Helper()
			return "0.0.0.0", "", t.TempDir()
		}, "but the allowlist is empty"},
		{"bad allowlist", func(t *testing.T, _ *installEnv) (string, string, string) {
			t.Helper()
			return "", "not-a-cidr", t.TempDir()
		}, "applying allowlist"},
		{"no admin account", func(t *testing.T, _ *installEnv) (string, string, string) {
			t.Helper()
			t.Setenv(admin.EnvUser, "")
			t.Setenv(admin.EnvPassword, "")
			orig := adminTerminal
			t.Cleanup(func() { adminTerminal = orig })
			adminTerminal = noTerminal
			return "", "", t.TempDir()
		}, "was not registered or started"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallEnv(t, "linux", false)
			host, cidrs, binDir := tc.setup(t, e)
			out, err := e.install(t, host, 0, cidrs, "", binDir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("install = %v, want %q\n%s", err, tc.want, out)
			}
			if len(e.register) != 0 {
				t.Errorf("service registered by a failed install")
			}
		})
	}
}

// TestInstall_AliasFailureIsAWarning: an alias that cannot be created is
// reported, and the install carries on.
func TestInstall_AliasFailureIsAWarning(t *testing.T) {
	e := newInstallEnv(t, "linux", false)
	binDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(binDir, openClawAlias, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := e.install(t, "", 0, "", "", binDir)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out, "Warning: could not create the openclaw alias") || len(e.register) != 1 {
		t.Errorf("output:\n%s", out)
	}
}

// TestUninstall covers the summary, the prompt and the removal on each
// service manager and mode.
func TestUninstall(t *testing.T) {
	for _, tc := range []struct {
		goos   string
		asRoot bool
		file   func(home string) string
		desc   string
	}{
		{"linux", false, userUnitPath, "systemd user service"},
		{"linux", true, func(string) string { return systemUnitPath }, "systemd system service"},
		{"darwin", false, userLaunchdPath, "launchd agent"},
		{"darwin", true, func(string) string { return systemLaunchdPath }, "launchd daemon"},
	} {
		t.Run(fmt.Sprintf("%s root=%v", tc.goos, tc.asRoot), func(t *testing.T) {
			h, log := svcHost(t, tc.goos)
			tu := &TargetUser{Username: "alice", UID: "1001", GID: "1001", HomeDir: "/home/alice", IsRoot: tc.asRoot}
			h.targetUser = func(string) (*TargetUser, error) { c := *tu; return &c, nil }
			h.detect = func(string) *ExistingInstall { return nil }
			t.Setenv(global.EnvVarHome, "")
			p := tc.file("/home/alice")
			writeHostFile(t, h, p, "service", 0o644)
			var err error
			out := captureStdout(t, func() { err = h.uninstall("", true) })
			if err != nil {
				t.Fatal(err)
			}
			if _, serr := os.Stat(h.path(p)); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("service file left: %v", serr)
			}
			for _, want := range []string{tc.desc + " (" + p + ")", "Service removed successfully.", "/home/alice/.claw"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if len(log.lines()) == 0 {
				t.Error("no service manager commands run")
			}
		})
	}
}

func TestUninstall_Paths(t *testing.T) {
	newUninstallHost := func(t *testing.T, goos string) (*host, *cmdLog) {
		t.Helper()
		h, log := svcHost(t, goos)
		h.targetUser = func(explicit string) (*TargetUser, error) {
			name := "alice"
			if explicit != "" {
				name = explicit
			}
			return &TargetUser{Username: name, UID: "1001", HomeDir: "/home/" + name, IsRoot: true}, nil
		}
		h.detect = func(string) *ExistingInstall { return nil }
		return h, log
	}
	t.Run("unsupported", func(t *testing.T) {
		h, _ := newUninstallHost(t, "plan9")
		if err := h.uninstall("", true); err == nil || !strings.Contains(err.Error(), "this is plan9") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no target user", func(t *testing.T) {
		h, _ := newUninstallHost(t, "linux")
		h.targetUser = func(string) (*TargetUser, error) { return nil, errors.New("not root") }
		if err := h.uninstall("", true); err == nil || err.Error() != "not root" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		h, log := newUninstallHost(t, "linux")
		writeHostFile(t, h, systemUnitPath, "unit", 0o644)
		var out string
		withStdin(t, "no\n", func() {
			out = captureStdout(t, func() {
				if err := h.uninstall("", false); err != nil {
					t.Error(err)
				}
			})
		})
		if !strings.Contains(out, "Uninstallation cancelled.") || len(log.lines()) != 0 {
			t.Errorf("output %q, commands %v", out, log.lines())
		}
		if _, err := os.Stat(h.path(systemUnitPath)); err != nil {
			t.Errorf("unit removed although cancelled: %v", err)
		}
	})
	t.Run("existing service account and data dir", func(t *testing.T) {
		h, _ := newUninstallHost(t, "linux")
		h.detect = func(string) *ExistingInstall { return &ExistingInstall{User: "carol", ClawHome: "/srv/claw"} }
		var err error
		out := captureStdout(t, func() { err = h.uninstall("", true) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "data in /srv/claw will NOT be deleted") {
			t.Errorf("output:\n%s", out)
		}
	})
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos+" removal fails", func(t *testing.T) {
			h, _ := newUninstallHost(t, goos)
			p := systemUnitPath
			if goos == "darwin" {
				p = systemLaunchdPath
			}
			writeHostFile(t, h, filepath.Join(p, "x"), "", 0o600)
			var err error
			captureStdout(t, func() { err = h.uninstall("", true) })
			if err == nil || !strings.Contains(err.Error(), "removing "+p) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestCommands_Flags: the install and uninstall commands carry their flags.
func TestCommands_Flags(t *testing.T) {
	inst := NewInstallCommand()
	for _, f := range []string{"host", "port", "allowed-cidrs", "user", "bin-dir", "yes"} {
		if inst.Flags().Lookup(f) == nil {
			t.Errorf("install lacks --%s", f)
		}
	}
	un := NewUninstallCommand()
	for _, f := range []string{"user", "yes"} {
		if un.Flags().Lookup(f) == nil {
			t.Errorf("uninstall lacks --%s", f)
		}
	}
}
