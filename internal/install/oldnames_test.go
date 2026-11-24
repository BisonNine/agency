package install

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The names of the release before the rename, which nothing may look at any
// more: an install under them is not seen, and install and uninstall touch
// only the current names.
const (
	oldName  = "claw"                // rename-guard:old-name
	oldLabel = "com.pivotllm.claweh" // rename-guard:old-name
)

// oldWord matches the old name anywhere but inside the openclaw alias.
var oldWord = regexp.MustCompile(`(?i)(^|[^n])` + oldName)

// writeOldInstall lays out every kind of install the old names had: system
// and user units, launchd plists, the system data directory with its binary,
// and the user's data directory.
func writeOldInstall(t *testing.T, h *host, home string) {
	t.Helper()
	for _, p := range []string{
		"/etc/systemd/system/" + oldName + ".service",
		filepath.Join(home, ".config", "systemd", "user", oldName+".service"),
		"/Library/LaunchDaemons/" + oldLabel + ".plist",
		filepath.Join(home, "Library", "LaunchAgents", oldLabel+".plist"),
		"/opt/" + oldName + "/bin/" + oldName,
		"/opt/" + oldName + "/config.json",
		filepath.Join(home, "."+oldName, "config.json"),
	} {
		writeHostFile(t, h, p, "[Service]\nExecStart=/opt/"+oldName+"/bin/"+oldName+"\n", 0o644)
	}
}

func TestDetectInstall_OldNamesNotSeen(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			h := fakeHost(t)
			h.goos = goos
			writeOldInstall(t, h, "/home/alice")
			if inst := h.detectInstall("/home/alice"); inst != nil {
				t.Errorf("detectInstall() = %+v, want nothing: an install under the old names is not seen", inst)
			}
		})
	}
}

// filesUnder lists the paths of every file and link below dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A user install writes the agency binary, the openclaw alias and the
// agency unit or plist, and none of them names the old name.
func TestInstall_WritesOnlyNewNames(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			e := newInstallEnv(t, goos, false)
			binDir := t.TempDir()
			if out, err := e.install(t, "", 0, "", "", binDir); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			if got := strings.Join(filesUnder(t, binDir), " "); got != "agency openclaw" {
				t.Errorf("installed into the bin dir: %s, want agency openclaw", got)
			}
			svc := userUnitPath(e.tu.HomeDir)
			if goos == "darwin" {
				svc = userLaunchdPath(e.tu.HomeDir)
			}
			for _, f := range filesUnder(t, e.tu.HomeDir) {
				if oldWord.MatchString(f) {
					t.Errorf("install wrote %s", f)
				}
			}
			body, err := os.ReadFile(svc)
			if err != nil {
				t.Fatalf("service file: %v", err)
			}
			if oldWord.Match(body) {
				t.Errorf("%s names the old name:\n%s", svc, body)
			}
			for _, line := range e.log.lines() {
				if oldWord.MatchString(line) {
					t.Errorf("install ran %q", line)
				}
			}
		})
	}
}

// Uninstall removes the agency unit or plist and leaves one under the old
// names alone.
func TestUninstall_RemovesOnlyNewNames(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, asRoot := range []bool{false, true} {
			h, log := svcHost(t, goos)
			tu := &TargetUser{Username: "alice", UID: "501", HomeDir: "/home/alice", IsRoot: asRoot}
			writeOldInstall(t, h, tu.HomeDir)
			current := userUnitPath(tu.HomeDir)
			switch {
			case goos == "linux" && asRoot:
				current = systemUnitPath
			case goos == "darwin":
				current = launchdDomainFor(tu).path
			}
			writeHostFile(t, h, current, "x", 0o644)
			var err error
			captureStdout(t, func() {
				if goos == "linux" {
					err = h.uninstallSystemd(tu)
				} else {
					err = h.uninstallLaunchd(tu)
				}
			})
			if err != nil {
				t.Fatalf("%s root=%v: uninstall: %v", goos, asRoot, err)
			}
			if h.exists(current) {
				t.Errorf("%s root=%v: %s was not removed", goos, asRoot, current)
			}
			left := strings.Join(filesUnder(t, h.root), "\n")
			if n := strings.Count(left, oldName+".service") + strings.Count(left, oldLabel+".plist"); n != 4 {
				t.Errorf("%s root=%v: old service files left = %d, want all 4:\n%s", goos, asRoot, n, left)
			}
			for _, line := range log.lines() {
				if oldWord.MatchString(line) {
					t.Errorf("%s root=%v: uninstall ran %q", goos, asRoot, line)
				}
			}
		}
	}
}
