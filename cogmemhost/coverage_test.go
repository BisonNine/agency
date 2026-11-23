// ClawEh - Cognitive Memory
// License: MIT

package cogmemhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/config"
)

func TestCheckRequiresWorkspace(t *testing.T) {
	if _, err := Check(testConfig(), "", "files/a.md"); err == nil || err.Error() != "no workspace configured" {
		t.Fatalf("err = %v", err)
	}
}

// A file that exists but cannot be read is reported, not loaded empty.
func TestLoaderReadFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	ws := setupWorkspace(t)
	write(t, ws, "files/locked.md", "secret")
	p := filepath.Join(ws, "files", "locked.md")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(p, 0o600); err != nil {
			t.Error(err)
		}
	})
	_, err := NewLoader(testConfig(), "alice", ws)("files/locked.md", 100)
	if err == nil || !strings.Contains(err.Error(), "cannot read files/locked.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestTruncateAtLine_SingleLongLine(t *testing.T) {
	if got := truncateAtLine("no newline here"); got != "no newline here" {
		t.Errorf("got %q", got)
	}
	if got := truncateAtLine("\nrest"); got != "\nrest" {
		t.Errorf("leading newline only: got %q", got)
	}
}

func TestMigrate_NoWorkspace(t *testing.T) {
	Migrate("alice", "", config.Modes{}) // nothing to open: returns without touching disk
}

func TestPruneSnapshots_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneSnapshots(f, time.Now(), time.Hour); err == nil {
		t.Error("dir is a file: want error")
	}

	if os.Geteuid() == 0 {
		return
	}
	dir := filepath.Join(t.TempDir(), "cogmem")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "cogmem.db.pre-v6.db")
	if err := os.WriteFile(snap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(snap, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	removed, err := PruneSnapshots(dir, time.Now(), time.Hour)
	if err == nil || len(removed) != 0 {
		t.Errorf("read-only dir = %v, %v; want an error and nothing removed", removed, err)
	}
}
