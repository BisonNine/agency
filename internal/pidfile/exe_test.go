package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.bison9.com/agency/global"
)

// fakeProc points procRoot at a temporary tree holding one process directory
// and selfExecutable at self, for the test's lifetime. exe is the link target
// of <pid>/exe ("" for none: unreadable), comm the content of <pid>/comm (""
// for none).
func fakeProc(t *testing.T, pid int, exe, comm, self string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if comm != "" {
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origRoot, origSelf := procRoot, selfExecutable
	t.Cleanup(func() { procRoot, selfExecutable = origRoot, origSelf })
	procRoot = root
	selfExecutable = func() (string, error) { return self, nil }
}

func TestIsOurExecutable(t *testing.T) {
	const pid = 4242
	installed := filepath.Join("/opt/agency/bin", global.InstalledBinaryName)
	tests := []struct {
		name            string
		exe, comm, self string
		want            bool
	}{
		// Started through the openclaw link: comm is "openclaw", but exe
		// resolves to the binary itself.
		{"started through a link", installed, "openclaw", installed, true},
		{"renamed binary, same file", "/usr/local/bin/alice-agency", "alice-agency", "/usr/local/bin/alice-agency", true},
		{"installed binary seen from a development build", installed, global.InstalledBinaryName, "/home/alice/src/build/" + global.InstalledBinaryName, true},
		{"replaced by an upgrade while running", installed + deletedSuffix, global.InstalledBinaryName, installed, true},
		{"a recycled pid running something else", "/usr/bin/sleep", "sleep", installed, false},
		{"exe unreadable: command name", "", global.InstalledBinaryName, "/x/alice-agency", true},
		{"exe unreadable: this binary's name", "", "alice-agency", "/x/alice-agency", true},
		{"exe unreadable: another program", "", "sleep", "/x/alice-agency", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeProc(t, pid, tt.exe, tt.comm, tt.self)
			if got := isOurExecutable(pid); got != tt.want {
				t.Errorf("isOurExecutable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsOurExecutable_NoProcfs: without /proc (macOS) existence, already
// checked by the caller, is the best answer.
func TestIsOurExecutable_NoProcfs(t *testing.T) {
	orig := procRoot
	t.Cleanup(func() { procRoot = orig })
	procRoot = filepath.Join(t.TempDir(), "absent")
	if !isOurExecutable(4242) {
		t.Error("without procfs a live process must count as ours")
	}
}

func TestCommName(t *testing.T) {
	if got := commName("a-very-long-binary-name"); got != "a-very-long-bin" {
		t.Errorf("commName = %q", got)
	}
	if got := commName("agency"); got != "agency" {
		t.Errorf("commName = %q", got)
	}
}
