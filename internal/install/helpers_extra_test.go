package install

import (
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/admin"
)

func TestDetectInstall_ByPlatform(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, systemUnitPath, "[Service]\nExecStart=/usr/local/bin/claw\nUser=alice\n", 0o644)
	writeHostFile(t, h, systemLaunchdPath, "<key>ProgramArguments</key><array><string>/usr/local/bin/claw</string></array>", 0o644)
	for goos, wantType := range map[string]string{"linux": "systemd-system", "darwin": "launchd-daemon"} {
		h.goos = goos
		inst := h.detectInstall("/home/alice")
		if inst == nil || inst.ServiceType != wantType {
			t.Errorf("%s: detected %+v, want %s", goos, inst, wantType)
		}
	}
	h.goos = "windows"
	if inst := h.detectInstall("/home/alice"); inst != nil {
		t.Errorf("windows: detected %+v", inst)
	}
}

func TestEnsureUserEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	tuFor := func(t *testing.T) *TargetUser {
		t.Helper()
		return &TargetUser{HomeDir: t.TempDir()}
	}

	t.Run("shell rc by shell", func(t *testing.T) {
		for shell, rc := range map[string]string{"/bin/zsh": ".zshrc", "/bin/bash": ".bashrc", "/bin/fish": ".profile", "": ".profile"} {
			t.Setenv("SHELL", shell)
			tu := tuFor(t)
			note := ensureUserEnv(tu, "/opt/tools/bin", filepath.Join(tu.HomeDir, global.DefaultDataDir))
			if !strings.Contains(note, "Added /opt/tools/bin to PATH in "+filepath.Join(tu.HomeDir, rc)) {
				t.Errorf("SHELL %q: note = %q", shell, note)
			}
		}
	})
	t.Run("nothing to add", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		tu := tuFor(t)
		home := filepath.Join(tu.HomeDir, global.DefaultDataDir)
		for _, binDir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
			if note := ensureUserEnv(tu, binDir, home); note != "" {
				t.Errorf("%s: note = %q", binDir, note)
			}
		}
		t.Setenv("PATH", "/usr/bin:/opt/on/path")
		if note := ensureUserEnv(tu, "/opt/on/path", home); note != "" {
			t.Errorf("dir on PATH: note = %q", note)
		}
		rc := filepath.Join(tu.HomeDir, ".bashrc")
		if err := os.WriteFile(rc, []byte("export PATH=/opt/in/rc:$PATH\nexport AGENCY_HOME=/srv/x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if note := ensureUserEnv(tu, "/opt/in/rc", "/srv/x"); note != "" {
			t.Errorf("already in rc: note = %q", note)
		}
		if _, err := os.Stat(filepath.Join(tu.HomeDir, ".profile")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("another rc written: %v", err)
		}
	})
	t.Run("only AGENCY_HOME", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		tu := tuFor(t)
		note := ensureUserEnv(tu, "/usr/local/bin", "/srv/claw")
		rc, err := os.ReadFile(filepath.Join(tu.HomeDir, ".bashrc"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(rc), "PATH") || !strings.Contains(string(rc), `export AGENCY_HOME="/srv/claw"`) {
			t.Errorf(".bashrc:\n%s", rc)
		}
		if !strings.HasPrefix(note, "Exported AGENCY_HOME=/srv/claw in ") || !strings.HasSuffix(note, "or open a new terminal.") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("rc unreadable", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		tu := tuFor(t)
		if err := os.Mkdir(filepath.Join(tu.HomeDir, ".bashrc"), 0o700); err != nil {
			t.Fatal(err)
		}
		if note := ensureUserEnv(tu, "/opt/x", "/srv/x"); !strings.HasPrefix(note, "Could not read ") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("rc not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		t.Setenv("SHELL", "/bin/bash")
		tu := tuFor(t)
		if err := os.Chmod(tu.HomeDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(tu.HomeDir, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if note := ensureUserEnv(tu, "/opt/x", "/srv/x"); !strings.HasPrefix(note, "Could not update ") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("home from the environment", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := userShellRC(""); got != filepath.Join(home, ".bashrc") {
			t.Errorf("userShellRC(\"\") = %q", got)
		}
		t.Setenv("HOME", "")
		if got := userShellRC(""); got != "" {
			t.Errorf("userShellRC without a home = %q", got)
		}
		if note := ensureUserEnv(&TargetUser{}, "/opt/x", "/srv/x"); note != "" {
			t.Errorf("note without a home = %q", note)
		}
	})
}

func TestChownToTarget(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chownToTarget(p, &TargetUser{Username: me.Username, UID: me.Uid, GID: me.Gid}); err != nil {
		t.Fatalf("chown to self: %v", err)
	}
	for name, tc := range map[string]struct {
		tu   *TargetUser
		path string
		want string
	}{
		"bad uid":     {&TargetUser{Username: "alice", UID: "x", GID: "1"}, p, `uid "x"`},
		"bad gid":     {&TargetUser{Username: "alice", UID: "1", GID: "y"}, p, `gid "y"`},
		"chown fails": {&TargetUser{Username: "alice", UID: me.Uid, GID: me.Gid}, p + ".missing", "chown "},
	} {
		if err := chownToTarget(tc.path, tc.tu); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestFixOwnership(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	tu := &TargetUser{UID: me.Uid, GID: me.Gid, IsRoot: true}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { fixOwnership(p, tu) }); out != "" {
		t.Errorf("chown to self warned: %q", out)
	}
	if out := captureStdout(t, func() { fixOwnership(p+".missing", tu) }); !strings.Contains(out, "Warning: could not change ownership of "+p+".missing") {
		t.Errorf("output = %q", out)
	}
	for _, skip := range []*TargetUser{nil, {UID: me.Uid, GID: me.Gid}, {UID: "x", GID: "y", IsRoot: true}} {
		if out := captureStdout(t, func() { fixOwnership(p+".missing", skip) }); out != "" {
			t.Errorf("%+v: output = %q", skip, out)
		}
	}
}

func TestProvisionAndRegister_Errors(t *testing.T) {
	t.Setenv(admin.EnvUser, "bob")
	t.Setenv(admin.EnvPassword, "bob's long password")
	t.Run("home blocked", func(t *testing.T) {
		stubInstallSeams(t, noTerminal)
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := provisionAndRegister(&TargetUser{}, "/x/claw", "/x", filepath.Join(f, "home"), false); err == nil || !strings.Contains(err.Error(), "creating ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("permissions unreadable", func(t *testing.T) {
		stubInstallSeams(t, noTerminal)
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, "config.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := provisionAndRegister(&TargetUser{}, "/x/claw", "/x", home, true); err == nil || !strings.Contains(err.Error(), "reading security.permissions") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("registration fails", func(t *testing.T) {
		stubInstallSeams(t, noTerminal)
		registerService = func(*TargetUser, string, string, string) error { return errors.New("systemctl missing") }
		var err error
		captureStdout(t, func() { _, err = provisionAndRegister(&TargetUser{}, "/x/claw", "/x", t.TempDir(), true) })
		if err == nil || err.Error() != "systemctl missing" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestApplyServerSettings_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyServerSettings("127.0.0.1", 0, filepath.Join(f, "home")); err == nil {
		t.Error("settings written under a file")
	}
	if os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.WriteFile(filepath.Join(ro, "config.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(ro, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		var err error
		captureStdout(t, func() { err = applyServerSettings("127.0.0.1", 1, ro) })
		if err == nil {
			t.Error("settings saved into a read-only home")
		}
	}
}

func TestServicePATH_Branches(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "/usr/sbin:/opt/tools/bin:/snap/bin:/opt/thinlinc/bin:/usr/local/sbin:/sbin")
	got := servicePATH(home, "/srv/bin")
	want := filepath.Join(home, ".local", "bin") + ":" + filepath.Join(home, "bin") + ":/srv/bin:/usr/local/bin:/usr/bin:/bin"
	if got != want {
		t.Errorf("servicePATH = %q, want %q", got, want)
	}
	// Without a home every PATH entry but the system and snap ones is kept.
	if got := servicePATH("", ""); got != "/opt/tools/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("servicePATH(\"\") = %q", got)
	}
}

func TestSmallHelpers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "claw")
	if err := copyBinary(p, p); err != nil {
		t.Errorf("copy onto itself: %v", err)
	}
	if err := copyBinary(filepath.Join(dir, "missing"), p); err == nil {
		t.Error("copy of a missing binary succeeded")
	}
	if ownedByUID(filepath.Join(dir, "missing"), "0") {
		t.Error("a missing path is owned by someone")
	}
	t.Setenv(global.EnvVarHome, "/srv/claw")
	if got := dataDir("/home/alice"); got != "/srv/claw" {
		t.Errorf("dataDir with AGENCY_HOME = %q", got)
	}
	if got := serviceManagerName("darwin"); got != "launchd" {
		t.Errorf("serviceManagerName(darwin) = %q", got)
	}
	// A user service without a known account still says how to proceed.
	err := checkExistingMode(&ExistingInstall{ServiceType: "systemd-user", ServicePath: "/home/x/.config/systemd/user/claw.service"}, true)
	if err == nil || !strings.Contains(err.Error(), "as the user it belongs to") {
		t.Errorf("err = %v", err)
	}
	plist := buildLaunchdPlist(launchdLabel, "alice", "staff", "/x/claw", "/Users/alice", "/x", "", false)
	if !strings.Contains(plist, "/Users/alice/.agency/logs/claw-launchd.log") {
		t.Errorf("plist without AGENCY_HOME logs elsewhere:\n%s", plist)
	}
	h := fakeHost(t)
	if got := h.ownerName("/missing"); got != "" {
		t.Errorf("ownerName of a missing path = %q", got)
	}
}

func TestPrintInstalledURLs(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"gateway":{"host":"127.0.0.1","port":18123}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { printInstalledURLs(home) }); !strings.Contains(out, "http://127.0.0.1:18123/") {
		t.Errorf("output = %q", out)
	}
	// Without a home the configured AGENCY_HOME is read.
	t.Setenv(global.EnvVarHome, home)
	if out := captureStdout(t, func() { printInstalledURLs("") }); !strings.Contains(out, "18123") {
		t.Errorf("output = %q", out)
	}
	// A failing writer stops the summary quietly.
	printListenerSummary(failWriter{}, config.DefaultConfig().Gateway)
	var buf bytes.Buffer
	printListenerSummary(&buf, config.DefaultConfig().Gateway)
	if buf.Len() == 0 {
		t.Error("no summary written")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
