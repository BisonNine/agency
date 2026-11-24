// Agency
// License: MIT

package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agency/config"
)

// The reader refuses before the loop has a registry, for an agent that is
// not configured, and when the agent's files folder is missing or can't be
// resolved.
func TestClassifierHost_InstructionsReaderRefusals(t *testing.T) {
	h := NewClassifierHost()
	h.Bind(&AgentLoop{cfg: &config.Config{}})
	if _, err := h.ReadInstructionsFile("alice", "files/x.md"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("no registry = %v", err)
	}

	host, ws := instructionsHost(t, []string{"file_read_bytes"})
	if _, err := host.ReadInstructionsFile("ghost", "files/x.md"); err == nil || err.Error() != "agent ghost is not running" {
		t.Errorf("unknown agent = %v", err)
	}

	files := filepath.Join(ws, "files")
	if err := os.RemoveAll(files); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReadInstructionsFile("alice", "files/x.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no files folder = %v, want ErrNotExist", err)
	}

	symlinkInstr(t, "files", files) // a link to itself: never resolves
	if _, err := host.ReadInstructionsDir("alice", "files"); err == nil || err.Error() != "the agent's files folder can't be read" {
		t.Errorf("files folder a link loop = %v", err)
	}
}

// A path out of the workspace is refused as one the agent may not read; a
// folder that can't be listed is reported as such.
func TestClassifierHost_InstructionsPathRefusals(t *testing.T) {
	host, ws := instructionsHost(t, []string{"file_read_bytes"})
	outOfWorkspace := ".."
	if _, err := host.ReadInstructionsDir("alice", outOfWorkspace); err == nil || err.Error() != "the agent may not read it" {
		t.Errorf("%s = %v", outOfWorkspace, err)
	}

	locked := filepath.Join(ws, "files", "classifiers", "inbox", "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("running with permission to list a 0000 folder")
	}
	if _, err := host.ReadInstructionsDir("alice", "files/classifiers/inbox/locked"); err == nil || err.Error() != "its files can't be listed" {
		t.Errorf("unlistable folder = %v", err)
	}
}
