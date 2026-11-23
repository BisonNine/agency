// ClawEh
// License: MIT

package folderwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
)

// A watched path that is not a folder is skipped: no marker, no notice.
func TestTick_SkipsPathThatIsNotAFolder(t *testing.T) {
	e := newWatchEnv(t)
	file := e.add(t, "plain.md")
	e.setFolders(config.AllowedFolderConfig{Path: file, Notify: true})
	e.watcher().tick()
	if _, err := os.Stat(markerPath(e.ws, "alice", file)); !os.IsNotExist(err) {
		t.Errorf("a file got a marker: %v", err)
	}
	if n := e.notices(); len(n) != 0 {
		t.Errorf("notices = %v", n)
	}
}

// Hidden folders are not walked, and an unreadable subfolder does not stop
// the scan.
func TestDetectNewFiles_SkipsHiddenAndUnreadableDirs(t *testing.T) {
	e := newWatchEnv(t)
	w := e.watcher()
	detect(w, e.ws, e.folder) // baseline

	if err := os.MkdirAll(filepath.Join(e.folder, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.folder, ".git", "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(e.folder, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	added := e.add(t, "new.md")

	got := detect(w, e.ws, e.folder)
	if len(got) != 1 || got[0] != added {
		t.Errorf("new files = %v, want only %s", got, added)
	}
}

func TestNotify_NoDefaultChannel(t *testing.T) {
	e := newWatchEnv(t)
	e.cfg.Bindings = nil
	w := e.watcher()
	w.notify(e.cfg, &e.cfg.Agents.List[0], e.folder, filepath.Join(e.folder, "x.md"))
	if n := e.notices(); len(n) != 0 {
		t.Errorf("notice sent with no default channel: %v", n)
	}
}

func TestNotify_ClosedBus(t *testing.T) {
	e := newWatchEnv(t)
	b := bus.NewMessageBus()
	b.Close()
	w := New(func() *config.Config { return e.cfg }, b, time.Hour, nil, e.state)
	w.notify(e.cfg, &e.cfg.Agents.List[0], e.folder, filepath.Join(e.folder, "x.md")) // logged, not blocked
}

func TestRemoveStaleMarkers_RemoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	e := newWatchEnv(t)
	w := e.watcher()
	marker := markerPath(dir, "alice", e.folder)
	detect(w, dir, e.folder) // writes the marker under dir
	markers := filepath.Dir(marker)
	if err := os.Chmod(markers, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(markers, 0o700) })
	removeStaleMarkers(markers, map[string]bool{})
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker gone despite a read-only folder: %v", err)
	}
}
