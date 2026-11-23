package install

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cmdLog records the service manager commands a host runs. A command line in
// fail fails with the output given there; every other command succeeds.
type cmdLog struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]string
}

func (c *cmdLog) run(name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, line)
	if out, ok := c.fail[line]; ok {
		return []byte(out), errors.New("exit status 1")
	}
	return []byte("ok"), nil
}

func (c *cmdLog) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// svcHost is a fakeHost for goos whose commands are recorded.
func svcHost(t *testing.T, goos string) (*host, *cmdLog) {
	t.Helper()
	h := fakeHost(t)
	h.goos = goos
	log := &cmdLog{fail: map[string]string{}}
	h.run = log.run
	return h, log
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Error(cerr)
		}
		done <- buf.String()
	}()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func readHostFile(t *testing.T, h *host, p string) string {
	t.Helper()
	b, err := os.ReadFile(h.path(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mkHostDir(t *testing.T, h *host, p string) {
	t.Helper()
	if err := os.MkdirAll(h.path(p), 0o755); err != nil {
		t.Fatal(err)
	}
}

func joinLines(s []string) string { return strings.Join(s, "\n") }

var (
	alice     = &TargetUser{Username: "alice", UID: "1001", GID: "1001", GroupName: "alice", HomeDir: "/home/alice"}
	aliceRoot = &TargetUser{Username: "alice", UID: "1001", GID: "1001", GroupName: "staff", HomeDir: "/home/alice", IsRoot: true}
)

func TestInstallSystemd_System(t *testing.T) {
	h, log := svcHost(t, "linux")
	mkHostDir(t, h, filepath.Dir(systemUnitPath))
	var err error
	out := captureStdout(t, func() { err = h.installSystemd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
	if err != nil {
		t.Fatal(err)
	}
	unit := readHostFile(t, h, systemUnitPath)
	for _, want := range []string{"User=alice\n", "Group=staff\n", "ExecStart=/usr/local/bin/claw\n", "CLAW_HOME=/opt/claw\n", "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	want := "systemctl daemon-reload\nsystemctl enable claw\nsystemctl restart claw"
	if got := joinLines(log.lines()); got != want {
		t.Errorf("commands:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(out, "Registered systemd system service: "+systemUnitPath+" (running as alice:staff)") {
		t.Errorf("output = %q", out)
	}
}

func TestInstallSystemd_SystemErrors(t *testing.T) {
	t.Run("unit not writable", func(t *testing.T) {
		h, log := svcHost(t, "linux")
		var err error
		captureStdout(t, func() { err = h.installSystemd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
		if err == nil || !strings.Contains(err.Error(), "writing systemd unit "+systemUnitPath) {
			t.Fatalf("err = %v", err)
		}
		if len(log.lines()) != 0 {
			t.Errorf("commands run after a failed write: %v", log.lines())
		}
	})
	for _, failing := range []string{"systemctl daemon-reload", "systemctl enable claw", "systemctl restart claw"} {
		t.Run(failing, func(t *testing.T) {
			h, log := svcHost(t, "linux")
			mkHostDir(t, h, filepath.Dir(systemUnitPath))
			log.fail[failing] = "unit broken"
			var err error
			captureStdout(t, func() { err = h.installSystemd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
			if err == nil || !strings.Contains(err.Error(), failing+": unit broken") {
				t.Fatalf("err = %v", err)
			}
			if l := log.lines(); l[len(l)-1] != failing {
				t.Errorf("commands continued after %q failed: %v", failing, l)
			}
		})
	}
}

func TestInstallSystemd_User(t *testing.T) {
	for _, linger := range []bool{true, false} {
		h, log := svcHost(t, "linux")
		if !linger {
			log.fail["loginctl enable-linger alice"] = "denied"
		}
		var err error
		out := captureStdout(t, func() { err = h.installSystemd(alice, "/home/alice/bin/claw", "/home/alice/bin", "/home/alice/.claw") })
		if err != nil {
			t.Fatal(err)
		}
		unit := readHostFile(t, h, userUnitPath("/home/alice"))
		if strings.Contains(unit, "User=") || !strings.Contains(unit, "ExecStart=/home/alice/bin/claw\n") || !strings.Contains(unit, "WantedBy=default.target") {
			t.Errorf("user unit:\n%s", unit)
		}
		want := "systemctl --user daemon-reload\nsystemctl --user enable claw\nsystemctl --user restart claw\nloginctl enable-linger alice"
		if got := joinLines(log.lines()); got != want {
			t.Errorf("commands:\n%s\nwant:\n%s", got, want)
		}
		wantOut := "Enabled systemd user lingering for alice"
		if !linger {
			wantOut = "Note: To allow user service to start at boot without login, run: loginctl enable-linger alice"
		}
		if !strings.Contains(out, wantOut) {
			t.Errorf("linger %v: output lacks %q:\n%s", linger, wantOut, out)
		}
	}
}

func TestInstallSystemd_UserErrors(t *testing.T) {
	t.Run("config folder blocked", func(t *testing.T) {
		h, _ := svcHost(t, "linux")
		writeHostFile(t, h, "/home/alice/.config", "", 0o600)
		var err error
		captureStdout(t, func() { err = h.installSystemd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "creating directory /home/alice/.config/systemd/user") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unit path is a folder", func(t *testing.T) {
		h, _ := svcHost(t, "linux")
		mkHostDir(t, h, userUnitPath("/home/alice"))
		var err error
		captureStdout(t, func() { err = h.installSystemd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "writing user systemd unit") {
			t.Fatalf("err = %v", err)
		}
	})
	for _, failing := range []string{"systemctl --user daemon-reload", "systemctl --user enable claw", "systemctl --user restart claw"} {
		t.Run(failing, func(t *testing.T) {
			h, log := svcHost(t, "linux")
			log.fail[failing] = "no session bus"
			var err error
			captureStdout(t, func() { err = h.installSystemd(alice, "/x/claw", "/x", "/home/alice/.claw") })
			if err == nil || !strings.Contains(err.Error(), failing+": no session bus") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestUninstallSystemd(t *testing.T) {
	t.Run("system", func(t *testing.T) {
		h, log := svcHost(t, "linux")
		writeHostFile(t, h, systemUnitPath, "[Unit]", 0o644)
		log.fail["systemctl disable --now claw"] = "not loaded" // best effort: ignored
		var err error
		out := captureStdout(t, func() { err = h.uninstallSystemd(aliceRoot) })
		if err != nil {
			t.Fatal(err)
		}
		if _, serr := os.Stat(h.path(systemUnitPath)); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("unit still there: %v", serr)
		}
		if got := joinLines(log.lines()); got != "systemctl disable --now claw\nsystemctl daemon-reload" {
			t.Errorf("commands = %q", got)
		}
		if !strings.Contains(out, "Removed systemd system service: "+systemUnitPath) {
			t.Errorf("output = %q", out)
		}
	})
	t.Run("user, nothing installed", func(t *testing.T) {
		h, log := svcHost(t, "linux")
		var err error
		captureStdout(t, func() { err = h.uninstallSystemd(alice) })
		if err != nil {
			t.Fatalf("uninstall of a missing unit: %v", err)
		}
		if got := joinLines(log.lines()); got != "systemctl --user disable --now claw\nsystemctl --user daemon-reload" {
			t.Errorf("commands = %q", got)
		}
	})
	for name, tu := range map[string]*TargetUser{"system": aliceRoot, "user": alice} {
		t.Run(name+" unit cannot be removed", func(t *testing.T) {
			h, _ := svcHost(t, "linux")
			p := systemUnitPath
			if !tu.IsRoot {
				p = userUnitPath(tu.HomeDir)
			}
			writeHostFile(t, h, filepath.Join(p, "x"), "", 0o600) // a non-empty folder
			var err error
			captureStdout(t, func() { err = h.uninstallSystemd(tu) })
			if err == nil || !strings.Contains(err.Error(), "removing "+p) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestInstallLaunchd(t *testing.T) {
	t.Run("daemon", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		mkHostDir(t, h, filepath.Dir(systemLaunchdPath))
		var err error
		out := captureStdout(t, func() { err = h.installLaunchd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
		if err != nil {
			t.Fatal(err)
		}
		plist := readHostFile(t, h, systemLaunchdPath)
		if !strings.Contains(plist, "<key>UserName</key>") || !strings.Contains(plist, "<string>/usr/local/bin/claw</string>") {
			t.Errorf("plist:\n%s", plist)
		}
		if fi, serr := os.Stat(h.path("/opt/claw/logs")); serr != nil || !fi.IsDir() {
			t.Errorf("logs folder not created: %v", serr)
		}
		want := "launchctl bootout system/" + launchdLabel + "\nlaunchctl unload -w " + systemLaunchdPath + "\nlaunchctl bootstrap system " + systemLaunchdPath
		if got := joinLines(log.lines()); got != want {
			t.Errorf("commands:\n%s\nwant:\n%s", got, want)
		}
		if !strings.Contains(out, "Loaded launchd system daemon "+launchdLabel) {
			t.Errorf("output = %q", out)
		}
	})
	t.Run("daemon falls back to load", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		mkHostDir(t, h, filepath.Dir(systemLaunchdPath))
		log.fail["launchctl bootstrap system "+systemLaunchdPath] = "old launchctl"
		var err error
		captureStdout(t, func() { err = h.installLaunchd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
		if err != nil {
			t.Fatal(err)
		}
		if l := log.lines(); l[len(l)-1] != "launchctl load -w "+systemLaunchdPath {
			t.Errorf("commands = %v", l)
		}
	})
	t.Run("daemon load fails", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		mkHostDir(t, h, filepath.Dir(systemLaunchdPath))
		log.fail["launchctl bootstrap system "+systemLaunchdPath] = "boot failed"
		log.fail["launchctl load -w "+systemLaunchdPath] = "load failed"
		var err error
		captureStdout(t, func() { err = h.installLaunchd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
		if err == nil || !strings.Contains(err.Error(), "boot failed / load failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("daemon plist not writable", func(t *testing.T) {
		h, _ := svcHost(t, "darwin")
		var err error
		captureStdout(t, func() { err = h.installLaunchd(aliceRoot, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
		if err == nil || !strings.Contains(err.Error(), "writing launchd daemon") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		var err error
		out := captureStdout(t, func() {
			err = h.installLaunchd(alice, "/Users/alice/bin/claw", "/Users/alice/bin", "/Users/alice/.claw")
		})
		if err != nil {
			t.Fatal(err)
		}
		dest := userLaunchdPath("/home/alice")
		if plist := readHostFile(t, h, dest); strings.Contains(plist, "<key>UserName</key>") {
			t.Errorf("agent plist names a user:\n%s", plist)
		}
		want := "launchctl bootout gui/1001/" + launchdLabel + "\nlaunchctl unload -w " + dest + "\nlaunchctl bootstrap gui/1001 " + dest
		if got := joinLines(log.lines()); got != want {
			t.Errorf("commands:\n%s\nwant:\n%s", got, want)
		}
		if !strings.Contains(out, "Loaded launchd user agent "+launchdLabel) {
			t.Errorf("output = %q", out)
		}
	})
	t.Run("agent load fails", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		dest := userLaunchdPath("/home/alice")
		log.fail["launchctl bootstrap gui/1001 "+dest] = "boot failed"
		var err error
		captureStdout(t, func() { err = h.installLaunchd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err != nil {
			t.Fatalf("fallback to load failed: %v", err)
		}
		log.fail["launchctl load -w "+dest] = "load failed"
		captureStdout(t, func() { err = h.installLaunchd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap gui/1001: boot failed / load failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent folder blocked", func(t *testing.T) {
		h, _ := svcHost(t, "darwin")
		writeHostFile(t, h, "/home/alice/Library", "", 0o600)
		var err error
		captureStdout(t, func() { err = h.installLaunchd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "creating directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("agent plist is a folder", func(t *testing.T) {
		h, _ := svcHost(t, "darwin")
		mkHostDir(t, h, userLaunchdPath("/home/alice"))
		var err error
		captureStdout(t, func() { err = h.installLaunchd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "writing launchd agent") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("logs folder blocked", func(t *testing.T) {
		h, _ := svcHost(t, "darwin")
		writeHostFile(t, h, "/home/alice/.claw/logs", "", 0o600)
		var err error
		captureStdout(t, func() { err = h.installLaunchd(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.Contains(err.Error(), "creating logs directory") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUninstallLaunchd(t *testing.T) {
	for name, tu := range map[string]*TargetUser{"daemon": aliceRoot, "agent": alice} {
		t.Run(name, func(t *testing.T) {
			h, log := svcHost(t, "darwin")
			p := systemLaunchdPath
			if !tu.IsRoot {
				p = userLaunchdPath(tu.HomeDir)
			}
			writeHostFile(t, h, p, "<plist/>", 0o644)
			var err error
			captureStdout(t, func() { err = h.uninstallLaunchd(tu) })
			if err != nil {
				t.Fatal(err)
			}
			if _, serr := os.Stat(h.path(p)); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("plist still there: %v", serr)
			}
			if len(log.lines()) != 2 {
				t.Errorf("commands = %v, want bootout and unload", log.lines())
			}
			// Removed again: nothing there is not an error.
			captureStdout(t, func() { err = h.uninstallLaunchd(tu) })
			if err != nil {
				t.Fatalf("second uninstall: %v", err)
			}
			writeHostFile(t, h, filepath.Join(p, "x"), "", 0o600)
			captureStdout(t, func() { err = h.uninstallLaunchd(tu) })
			if err == nil || !strings.Contains(err.Error(), "removing "+p) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestIsLaunchdServiceActive(t *testing.T) {
	h, log := svcHost(t, "darwin")
	h.run = func(name string, args ...string) ([]byte, error) {
		if _, cerr := log.run(name, args...); cerr != nil {
			t.Error(cerr)
		}
		return []byte("PID\tStatus\tLabel\n12\t0\t" + launchdLabel + "\n"), nil
	}
	if !h.isLaunchdServiceActive(launchdLabel) {
		t.Error("listed service reported inactive")
	}
	if h.isLaunchdServiceActive("com.example.other") {
		t.Error("unlisted service reported active")
	}
	h.run = func(string, ...string) ([]byte, error) { return nil, errors.New("no launchctl") }
	if h.isLaunchdServiceActive(launchdLabel) {
		t.Error("service reported active when launchctl fails")
	}
}

func TestRegister(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		h, log := svcHost(t, "linux")
		var err error
		captureStdout(t, func() { err = h.register(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err != nil || !strings.HasPrefix(joinLines(log.lines()), "systemctl --user") {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
		log.fail["systemctl --user daemon-reload"] = "x"
		captureStdout(t, func() { err = h.register(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.HasPrefix(err.Error(), "installing systemd service: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("darwin", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		var err error
		captureStdout(t, func() { err = h.register(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err != nil || !strings.HasPrefix(joinLines(log.lines()), "launchctl") {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
		if ierr := os.RemoveAll(h.path("/home/alice/.claw/logs")); ierr != nil {
			t.Fatal(ierr)
		}
		writeHostFile(t, h, "/home/alice/.claw/logs", "", 0o600)
		captureStdout(t, func() { err = h.register(alice, "/x/claw", "/x", "/home/alice/.claw") })
		if err == nil || !strings.HasPrefix(err.Error(), "installing launchd service: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other", func(t *testing.T) {
		h, log := svcHost(t, "freebsd")
		if err := h.register(alice, "/x/claw", "/x", "/home/alice/.claw"); err != nil || len(log.lines()) != 0 {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
	})
}

func TestResolveTargetUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	setEUID := func(t *testing.T, id int) {
		t.Helper()
		orig := geteuid
		t.Cleanup(func() { geteuid = orig })
		geteuid = func() int { return id }
	}
	t.Run("regular user", func(t *testing.T) {
		setEUID(t, 1000)
		for _, explicit := range []string{"", me.Username} {
			tu, err := resolveTargetUser(explicit)
			if err != nil || tu.Username != me.Username || tu.UID != me.Uid || tu.IsRoot || tu.GroupName == "" {
				t.Fatalf("resolveTargetUser(%q) = %+v, %v", explicit, tu, err)
			}
		}
		if _, err := resolveTargetUser("someone-else-x"); err == nil || !strings.Contains(err.Error(), "without root privileges") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("root", func(t *testing.T) {
		setEUID(t, 0)
		t.Setenv("SUDO_USER", me.Username)
		tu, err := resolveTargetUser("")
		if err != nil || tu.Username != me.Username || !tu.IsRoot {
			t.Fatalf("resolveTargetUser via SUDO_USER = %+v, %v", tu, err)
		}
		if _, err := resolveTargetUser("no-such-user-x"); err == nil || !strings.Contains(err.Error(), `lookup user "no-such-user-x"`) {
			t.Fatalf("err = %v", err)
		}
		for _, sudoUser := range []string{"", "root"} {
			t.Setenv("SUDO_USER", sudoUser)
			if _, err := resolveTargetUser(""); err == nil || !strings.Contains(err.Error(), "must run as a regular user, not root") {
				t.Fatalf("SUDO_USER %q: err = %v", sudoUser, err)
			}
		}
	})
}

func TestConfirmPrompt(t *testing.T) {
	if ok, err := confirmPrompt("go?", true); !ok || err != nil {
		t.Fatalf("auto yes = %v, %v", ok, err)
	}
	for input, want := range map[string]bool{"y\n": true, "  YES\n": true, "n\n": false, "": false} {
		var ok bool
		withStdin(t, input, func() {
			out := captureStdout(t, func() {
				var err error
				if ok, err = confirmPrompt("go?", false); err != nil {
					t.Error(err)
				}
			})
			if out != "go? [y/N]: " {
				t.Errorf("prompt = %q", out)
			}
		})
		if ok != want {
			t.Errorf("answer %q = %v, want %v", input, ok, want)
		}
	}
}

// withStdin runs fn with os.Stdin reading input.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if _, cerr := io.WriteString(w, input); cerr != nil {
			t.Error(cerr)
		}
		if cerr := w.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	os.Stdin = r
	defer func() {
		os.Stdin = old
		if cerr := r.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	fn()
}
