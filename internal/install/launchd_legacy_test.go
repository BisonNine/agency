package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
)

// legacyPlist is a plist as earlier releases wrote it: the legacy label,
// the binary, the account and the data directory.
func legacyPlist(bin, clawHome string) string {
	return strings.Replace(
		buildLaunchdPlist(legacyLaunchdLabel, "alice", "staff", bin, "/Users/alice", filepath.Dir(bin), clawHome, true),
		"\t<key>RunAtLoad</key>", "\t<key>Custom</key>\n\t<string>kept</string>\n\t<key>RunAtLoad</key>", 1)
}

// The current and legacy launchd domains of alice: her agent, and the system
// daemon when run as root.
var launchdDomainCases = map[string]*TargetUser{"daemon": aliceRoot, "agent": alice}

func assertGone(t *testing.T, h *host, p string) {
	t.Helper()
	if _, err := os.Stat(h.path(p)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still there: %v", p, err)
	}
}

// installLaunchd over a service registered under the legacy label unloads it
// and removes its plist before loading the new one; a second run finds
// nothing left to move.
func TestInstallLaunchd_MovesLegacyService(t *testing.T) {
	for name, tu := range launchdDomainCases {
		t.Run(name, func(t *testing.T) {
			h, log := svcHost(t, "darwin")
			d := launchdDomainFor(tu)
			mkHostDir(t, h, filepath.Dir(d.path))
			writeHostFile(t, h, d.legacyPath, legacyPlist("/usr/local/bin/claw", "/opt/claw"), 0o644)

			var err error
			out := captureStdout(t, func() { err = h.installLaunchd(tu, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
			if err != nil {
				t.Fatal(err)
			}
			assertGone(t, h, d.legacyPath)
			if plist := readHostFile(t, h, d.path); !strings.Contains(plist, "<string>"+launchdLabel+"</string>") {
				t.Errorf("new plist:\n%s", plist)
			}
			want := []string{
				"launchctl bootout " + d.target + "/" + legacyLaunchdLabel,
				"launchctl unload -w " + d.legacyPath,
				"launchctl bootout " + d.target + "/" + launchdLabel,
				"launchctl unload -w " + d.path,
				"launchctl bootstrap " + d.target + " " + d.path,
			}
			if got := joinLines(log.lines()); got != joinLines(want) {
				t.Errorf("commands:\n%s\nwant:\n%s", got, joinLines(want))
			}
			if !strings.Contains(out, "Removed the service registered as "+legacyLaunchdLabel) {
				t.Errorf("output = %q", out)
			}

			log.calls = nil
			captureStdout(t, func() { err = h.installLaunchd(tu, "/usr/local/bin/claw", "/usr/local/bin", "/opt/claw") })
			if err != nil {
				t.Fatal(err)
			}
			if got := joinLines(log.lines()); strings.Contains(got, legacyLaunchdLabel) {
				t.Errorf("second install touched the legacy label:\n%s", got)
			}
		})
	}
}

// A legacy plist that cannot be removed fails the move before the new
// service is loaded, so two services never run on one data directory.
func TestMigrateLegacyLaunchd_LegacyNotRemovable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove the file anyway")
	}
	h, log := svcHost(t, "darwin")
	d := launchdDomainFor(alice)
	writeHostFile(t, h, d.legacyPath, "legacy", 0o644)
	writeHostFile(t, h, d.path, "current", 0o644)
	dir := h.path(filepath.Dir(d.legacyPath))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(dir, 0o755); cerr != nil {
			t.Error(cerr)
		}
	})
	var err error
	captureStdout(t, func() { err = h.migrateLegacyLaunchd(alice) })
	if err == nil || !strings.Contains(err.Error(), "removing "+d.legacyPath) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(joinLines(log.lines()), "bootstrap") {
		t.Errorf("new service loaded: %v", log.lines())
	}
}

// migrateLegacyLaunchd, which upgrade runs, rewrites the legacy plist under
// the current label with everything else unchanged, removes the legacy one
// and loads the new; running it again changes nothing.
func TestMigrateLegacyLaunchd(t *testing.T) {
	for name, tu := range launchdDomainCases {
		t.Run(name, func(t *testing.T) {
			h, log := svcHost(t, "darwin")
			d := launchdDomainFor(tu)
			old := legacyPlist("/Users/alice/bin/claw", "/Users/alice/data")
			writeHostFile(t, h, d.legacyPath, old, 0o644)

			var err error
			out := captureStdout(t, func() { err = h.migrateLegacyLaunchd(tu) })
			if err != nil {
				t.Fatal(err)
			}
			assertGone(t, h, d.legacyPath)
			wantPlist := strings.Replace(old, "<string>"+legacyLaunchdLabel+"</string>", "<string>"+launchdLabel+"</string>", 1)
			if got := readHostFile(t, h, d.path); got != wantPlist {
				t.Errorf("new plist:\n%s\nwant:\n%s", got, wantPlist)
			}
			want := []string{
				"launchctl bootout " + d.target + "/" + legacyLaunchdLabel,
				"launchctl unload -w " + d.legacyPath,
				"launchctl bootout " + d.target + "/" + launchdLabel,
				"launchctl unload -w " + d.path,
				"launchctl bootstrap " + d.target + " " + d.path,
			}
			if got := joinLines(log.lines()); got != joinLines(want) {
				t.Errorf("commands:\n%s\nwant:\n%s", got, joinLines(want))
			}
			if !strings.Contains(out, "The service is now registered as "+launchdLabel) {
				t.Errorf("output = %q", out)
			}

			log.calls = nil
			captureStdout(t, func() { err = h.migrateLegacyLaunchd(tu) })
			if err != nil || len(log.lines()) != 0 {
				t.Errorf("second run: err = %v, commands %v", err, log.lines())
			}
			if got := readHostFile(t, h, d.path); got != wantPlist {
				t.Errorf("second run changed the plist:\n%s", got)
			}
		})
	}
}

func TestMigrateLegacyLaunchd_Cases(t *testing.T) {
	d := launchdDomainFor(alice)
	t.Run("only the current label", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		writeHostFile(t, h, d.path, "current", 0o644)
		if err := h.migrateLegacyLaunchd(alice); err != nil || len(log.lines()) != 0 {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
		if got := readHostFile(t, h, d.path); got != "current" {
			t.Errorf("plist = %q", got)
		}
	})
	t.Run("both labels keep the current plist", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		writeHostFile(t, h, d.path, "current", 0o644)
		writeHostFile(t, h, d.legacyPath, legacyPlist("/x/claw", "/x/data"), 0o644)
		var err error
		captureStdout(t, func() { err = h.migrateLegacyLaunchd(alice) })
		if err != nil {
			t.Fatal(err)
		}
		assertGone(t, h, d.legacyPath)
		if got := readHostFile(t, h, d.path); got != "current" {
			t.Errorf("plist = %q", got)
		}
		if l := log.lines(); l[len(l)-1] != "launchctl bootstrap "+d.target+" "+d.path {
			t.Errorf("commands = %v", l)
		}
	})
	t.Run("nothing installed", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		if err := h.migrateLegacyLaunchd(alice); err != nil || len(log.lines()) != 0 {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
	})
	t.Run("not macOS", func(t *testing.T) {
		h, log := svcHost(t, "linux")
		writeHostFile(t, h, d.legacyPath, "legacy", 0o644)
		if err := h.migrateLegacyLaunchd(alice); err != nil || len(log.lines()) != 0 {
			t.Fatalf("err = %v, commands %v", err, log.lines())
		}
	})
	t.Run("new plist not writable", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		writeHostFile(t, h, d.legacyPath, "legacy", 0o644)
		mkHostDir(t, h, filepath.Join(d.path, "x")) // a folder where the plist goes
		err := h.migrateLegacyLaunchd(alice)
		if err == nil || !strings.Contains(err.Error(), "writing "+d.path) {
			t.Fatalf("err = %v", err)
		}
		if len(log.lines()) != 0 {
			t.Errorf("legacy service unloaded although the new plist was not written: %v", log.lines())
		}
		if got := readHostFile(t, h, d.legacyPath); got != "legacy" {
			t.Errorf("legacy plist = %q", got)
		}
	})
	t.Run("load fails", func(t *testing.T) {
		h, log := svcHost(t, "darwin")
		writeHostFile(t, h, d.legacyPath, "legacy", 0o644)
		log.fail["launchctl bootstrap "+d.target+" "+d.path] = "boot failed"
		log.fail["launchctl load -w "+d.path] = "load failed"
		var err error
		captureStdout(t, func() { err = h.migrateLegacyLaunchd(alice) })
		if err == nil || !strings.Contains(err.Error(), "boot failed / load failed") {
			t.Fatalf("err = %v", err)
		}
	})
}

// On any system but macOS the exported entry point does nothing.
func TestMigrateLegacyLaunchd_NotDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("would act on this machine's launchd")
	}
	if err := MigrateLegacyLaunchd(); err != nil {
		t.Fatal(err)
	}
}

// uninstall removes the service under either label, or both.
func TestUninstallLaunchd_Legacy(t *testing.T) {
	for name, tu := range launchdDomainCases {
		for _, both := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s both=%v", name, both), func(t *testing.T) {
				h, log := svcHost(t, "darwin")
				d := launchdDomainFor(tu)
				writeHostFile(t, h, d.legacyPath, "legacy", 0o644)
				if both {
					writeHostFile(t, h, d.path, "current", 0o644)
				}
				var err error
				captureStdout(t, func() { err = h.uninstallLaunchd(tu) })
				if err != nil {
					t.Fatal(err)
				}
				assertGone(t, h, d.path)
				assertGone(t, h, d.legacyPath)
				got := joinLines(log.lines())
				for _, want := range []string{
					"launchctl bootout " + d.target + "/" + launchdLabel,
					"launchctl bootout " + d.target + "/" + legacyLaunchdLabel,
				} {
					if !strings.Contains(got, want) {
						t.Errorf("commands lack %q:\n%s", want, got)
					}
				}
			})
		}
	}
}

// A legacy service is reported active when launchd lists the legacy label.
func TestDetectDarwinInstall_LegacyActive(t *testing.T) {
	h := fakeHost(t)
	h.run = func(string, ...string) ([]byte, error) {
		return []byte("PID\tStatus\tLabel\n12\t0\t" + legacyLaunchdLabel + "\n"), nil
	}
	writeHostFile(t, h, legacyUserLaunchdPath("/Users/alice"), plistDaemon, 0o644)
	inst := h.detectDarwinInstall("/Users/alice")
	if inst == nil || !inst.IsActive || inst.ServicePath != legacyUserLaunchdPath("/Users/alice") {
		t.Fatalf("detectDarwinInstall() = %+v", inst)
	}
}

// A user install over a legacy agent moves it: the new plist keeps the
// binary location and data directory, and the legacy one is gone.
func TestInstall_DarwinMovesLegacyAgent(t *testing.T) {
	e := newInstallEnv(t, "darwin", false)
	t.Setenv(global.EnvVarHome, "")
	t.Setenv("CLAW_HOME", "")
	binDir := t.TempDir()
	bin := filepath.Join(binDir, global.InstalledBinaryName)
	legacyPath := legacyUserLaunchdPath(e.tu.HomeDir)
	writeHostFile(t, e.h, legacyPath, legacyPlist(bin, e.clawHome), 0o644)
	e.existing = e.h.parseLaunchdPlist(legacyPath, "launchd-agent")

	out, err := e.install(t, "", 0, "", "", "")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	assertGone(t, e.h, legacyPath)
	newPath := userLaunchdPath(e.tu.HomeDir)
	plist := readHostFile(t, e.h, newPath)
	for _, want := range []string{
		"<string>" + launchdLabel + "</string>",
		"<string>" + bin + "</string>",
		"<key>CLAW_HOME</key>\n\t\t<string>" + e.clawHome + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if !strings.Contains(joinLines(e.log.lines()), "launchctl bootout gui/"+e.tu.UID+"/"+legacyLaunchdLabel) {
		t.Errorf("legacy service not unloaded: %v", e.log.lines())
	}

	// Again, now finding the moved service: the legacy label is not touched.
	e.existing = e.h.parseLaunchdPlist(newPath, "launchd-agent")
	e.log.calls = nil
	if out, err = e.install(t, "", 0, "", "", ""); err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if strings.Contains(joinLines(e.log.lines()), legacyLaunchdLabel) {
		t.Errorf("second install touched the legacy label: %v", e.log.lines())
	}
}

// The uninstall summary names the legacy plist when only that one exists.
func TestUninstall_DarwinNamesLegacyPlist(t *testing.T) {
	e := newInstallEnv(t, "darwin", false)
	legacyPath := legacyUserLaunchdPath(e.tu.HomeDir)
	writeHostFile(t, e.h, legacyPath, "legacy", 0o644)
	var err error
	out := captureStdout(t, func() { err = e.h.uninstall("", true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "launchd agent ("+legacyPath+")") {
		t.Errorf("output:\n%s", out)
	}
	assertGone(t, e.h, legacyPath)
}
