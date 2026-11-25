package pidfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// noProcfs points procRoot at a folder that does not exist, as on macOS, and
// selfExecutable at self; ps answers with out and err.
func noProcfs(t *testing.T, self, out string, err error) {
	t.Helper()
	origRoot, origSelf, origPS := procRoot, selfExecutable, psOutput
	t.Cleanup(func() { procRoot, selfExecutable, psOutput = origRoot, origSelf, origPS })
	procRoot = filepath.Join(t.TempDir(), "absent")
	selfExecutable = func() (string, error) { return self, nil }
	psOutput = func(int, string) (string, error) { return out, err }
}

// Without /proc (macOS) the executable comes from ps, so a recycled pid
// running something else is not ours.
func TestIsOurExecutable_NoProcfs(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "alice-agency")
	if err := os.WriteFile(self, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "openclaw")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"this binary", self, nil, true},
		{"started through a link", link, nil, true},
		{"installed binary seen from a development build", "/opt/agency/bin/" + global.InstalledBinaryName, nil, true},
		{"this binary's name, relative", "./alice-agency", nil, true},
		{"a recycled pid running something else", "/bin/sleep", nil, false},
		{"no output", "", nil, false},
		{"the process is gone", "", &exec.ExitError{}, false},
		{"ps cannot be run", "", exec.ErrNotFound, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			noProcfs(t, self, tt.out, tt.err)
			if got := isOurExecutable(4242); got != tt.want {
				t.Errorf("isOurExecutable = %v, want %v", got, tt.want)
			}
		})
	}
}

// A /proc entry that cannot be read on a system that has /proc: existence,
// already checked by the caller, is the best answer.
func TestIsOurExecutable_UnreadableEntry(t *testing.T) {
	fakeProc(t, 4242, "", "", "/x/alice-agency")
	if !isOurExecutable(4242) {
		t.Error("a live process whose entry cannot be read must count as ours")
	}
}

func TestRSSBytes_NoProcfs(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want int64
		ok   bool
	}{
		{"resident set", "2048", nil, 2048 * 1024, true},
		{"the process is gone", "", &exec.ExitError{}, 0, false},
		{"not a number", "lots", nil, 0, false},
		{"zero", "0", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			noProcfs(t, "/x/alice-agency", tt.out, tt.err)
			got, ok := RSSBytes(4242)
			if got != tt.want || ok != tt.ok {
				t.Errorf("RSSBytes = %d, %v, want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// The real ps, as used on macOS: this process is found and is ours, and a
// pid that cannot exist is not.
func TestPS_ThisProcess(t *testing.T) {
	if _, err := exec.LookPath(psCommand()); err != nil {
		t.Skip("no ps on this system")
	}
	orig := procRoot
	t.Cleanup(func() { procRoot = orig })
	procRoot = filepath.Join(t.TempDir(), "absent")
	if runtime.GOOS == "darwin" {
		t.Setenv("PATH", "") // ps is run by its full path
	}

	if !isOurExecutable(os.Getpid()) {
		t.Error("this process is not recognised as ours")
	}
	if rss, ok := RSSBytes(os.Getpid()); !ok || rss <= 0 {
		t.Errorf("RSSBytes = %d, %v, want a positive size", rss, ok)
	}
	if isOurExecutable(4194305) {
		t.Error("a pid that cannot exist counted as ours")
	}
	if _, ok := RSSBytes(4194305); ok {
		t.Error("RSSBytes reported a size for a pid that cannot exist")
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
