// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"go.bison9.com/agency/forum/forumfs"
)

// covChmod sets path's mode and restores 0o700 at cleanup, so t.TempDir
// can remove what the test locked down. Under root, which ignores modes,
// the test is skipped.
func covChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

// covWantErr fails unless err is non-nil and contains every substring.
func covWantErr(t *testing.T, what string, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want an error containing %q, got nil", what, subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%s: error %q lacks %q", what, err, s)
		}
	}
}

// covWriteFile writes a file, failing the test on error.
func covWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCreateStoreRefusals(t *testing.T) {
	if _, err := createStore("relative", uuid.NewString()); err == nil {
		t.Error("relative base: want an error")
	}
	if _, err := createStore(t.TempDir(), "Not-A-UUID"); err == nil {
		t.Error("bad forum ID: want an error")
	}

	fileBase := filepath.Join(t.TempDir(), "file")
	covWriteFile(t, fileBase, "x")
	_, err := createStore(fileBase, uuid.NewString())
	covWantErr(t, "base is a file", err, "create forum store")
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("base is a file: %v is not ErrExist", err)
	}

	// A base under a file cannot be created either.
	_, err = createStore(filepath.Join(fileBase, "sub"), uuid.NewString())
	covWantErr(t, "base under a file", err, "create forum store")

	base := t.TempDir()
	id := uuid.NewString()
	if _, err := createStore(base, id); err != nil {
		t.Fatalf("createStore: %v", err)
	}
	_, err = createStore(base, id)
	covWantErr(t, "forum exists", err, "create forum store")
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("forum exists: %v is not ErrExist", err)
	}
}

func TestOpenStoreLayoutErrors(t *testing.T) {
	if _, err := openStore("relative", uuid.NewString()); err == nil {
		t.Error("relative base: want an error")
	}
	s := stNewStore(t)
	cfgPath := filepath.Join(s.dir, fileConfig)

	// forum.json that is a directory is corrupt.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfgPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(s.base, s.id); !errors.Is(err, ErrCorrupt) {
		t.Errorf("forum.json a directory: err = %v, want ErrCorrupt", err)
	}

	// An unreadable forum directory is neither missing nor corrupt.
	covChmod(t, s.dir, 0)
	_, err := openStore(s.base, s.id)
	covWantErr(t, "forum dir unreadable", err, "open forum store")
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		t.Errorf("forum dir unreadable: err = %v", err)
	}

	// An unreadable base likewise.
	covChmod(t, s.base, 0)
	_, err = openStore(s.base, s.id)
	covWantErr(t, "base unreadable", err, "open forum store")
}

func TestListForumsErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if ids, err := listIncomplete(missing); err != nil || len(ids) != 0 {
		t.Errorf("listIncomplete(missing) = %v, %v", ids, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	covWriteFile(t, file, "x")
	_, err := listForums(file)
	covWantErr(t, "listForums(file)", err, "list forums")
	_, err = listIncomplete(file)
	covWantErr(t, "listIncomplete(file)", err, "list forums")
}

func TestRunsErrors(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	// Entries that are not run directories are skipped.
	covWriteFile(t, filepath.Join(s.dir, dirRuns, "7"), "a file, not a run")
	if err := os.Mkdir(filepath.Join(s.dir, dirRuns, "07"), 0o700); err != nil {
		t.Fatal(err)
	}
	runs, err := f.Runs()
	if err != nil || len(runs) != 1 || runs[0] != 1 {
		t.Errorf("Runs = %v, %v; want [1]", runs, err)
	}
	if err := os.RemoveAll(filepath.Join(s.dir, dirRuns)); err != nil {
		t.Fatal(err)
	}
	_, err = f.Runs()
	covWantErr(t, "runs/ missing", err, "list runs of forum")
}

func TestCreateRunErrors(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	_, err := f.CreateRun(0)
	covWantErr(t, "run 0", err, "bad run number 0")

	_, err = f.CreateRun(1)
	covWantErr(t, "run exists", err, "create run 1")
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("run exists: %v is not ErrExist", err)
	}

	if err := os.RemoveAll(filepath.Join(s.dir, dirRuns)); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, filepath.Join(s.dir, dirRuns), "not a directory")
	_, err = f.CreateRun(2)
	covWantErr(t, "runs/ a file", err, "create run 2", "runs is not a directory")
}

func TestCreateRunRunsSymlink(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	elsewhere := t.TempDir()
	if err := os.RemoveAll(filepath.Join(s.dir, dirRuns)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(s.dir, dirRuns)); err != nil {
		t.Fatal(err)
	}
	_, err := f.CreateRun(2)
	covWantErr(t, "runs/ a symlink", err, "runs is a symbolic link")
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("CreateRun wrote through the link: %v", entries)
	}
}

func TestOpenRunErrors(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	if _, err := f.OpenRun(0); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenRun(0) = %v, want ErrNotFound", err)
	}
	covWriteFile(t, filepath.Join(s.dir, dirRuns, "2"), "x")
	if _, err := f.OpenRun(2); !errors.Is(err, ErrCorrupt) {
		t.Errorf("run a file: err = %v, want ErrCorrupt", err)
	}
	covChmod(t, filepath.Join(s.dir, dirRuns), 0)
	_, err := f.OpenRun(1)
	covWantErr(t, "runs unreadable", err, "open run 1")
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		t.Errorf("runs unreadable: err = %v", err)
	}
}

func TestRemoveRunErrors(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	covWantErr(t, "forum handle", f.RemoveRun(), "not a run")

	// A run already gone is removed.
	if err := f.Run(5).RemoveRun(); err != nil {
		t.Errorf("RemoveRun of a missing run: %v", err)
	}

	covChmod(t, filepath.Join(s.dir, dirRuns), 0o500)
	covWantErr(t, "runs read-only", s.RemoveRun(), "remove run 1")
	if _, err := os.Stat(s.root); err != nil {
		t.Errorf("the run is gone after a failed removal: %v", err)
	}
}

func TestSyncDirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	covWantErr(t, "syncDir", syncDir(missing), "sync "+missing)
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	covWantErr(t, "syncDirAt", syncDirAt(r, "missing"), "sync missing")
}

func TestMkdirAllExactExisting(t *testing.T) {
	base := t.TempDir()
	if err := mkdirAllExact(base, 0o700); err != nil {
		t.Errorf("existing directory: %v", err)
	}
	file := filepath.Join(base, "f")
	covWriteFile(t, file, "x")
	if err := mkdirAllExact(file, 0o700); !errors.Is(err, fs.ErrExist) {
		t.Errorf("file: err = %v, want ErrExist", err)
	}
	if err := mkdirAllExact(filepath.Join(file, "a", "b"), 0o700); !errors.Is(err, fs.ErrExist) {
		t.Errorf("under a file: err = %v, want ErrExist", err)
	}
	locked := filepath.Join(base, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, locked, 0o500)
	if err := mkdirAllExact(filepath.Join(locked, "x"), 0o700); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("read-only parent: err = %v, want ErrPermission", err)
	}
}

// covRoot opens a fresh temporary directory as an os.Root.
func covRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

func TestMkdirAtRefusals(t *testing.T) {
	r, dir := covRoot(t)
	if err := mkdirAt(r, "../x", defaultPerms); err == nil {
		t.Error("escaping path: want an error")
	}
	if err := mkdirAt(r, ".", defaultPerms); err != nil {
		t.Errorf("root itself: %v", err)
	}
	covWriteFile(t, filepath.Join(dir, "file"), "x")
	covWantErr(t, "file component", mkdirAt(r, "file/sub", defaultPerms), "file is not a directory")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "symlink component", mkdirAt(r, "link/sub", defaultPerms), "link is a symbolic link")
	if err := os.Mkdir(filepath.Join(dir, "closed"), 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, filepath.Join(dir, "closed"), 0)
	if err := mkdirAt(r, "closed/sub/deeper", defaultPerms); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("unreadable component: err = %v, want ErrPermission", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "ro"), 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, filepath.Join(dir, "ro"), 0o500)
	if err := mkdirAt(r, "ro/sub", defaultPerms); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("read-only parent: err = %v, want ErrPermission", err)
	}
}

func TestNoSymlinksRefusals(t *testing.T) {
	r, dir := covRoot(t)
	covWriteFile(t, filepath.Join(dir, "file"), "x")
	covWantErr(t, "file component", lstatRegular(r, "file/x"), "file is not a directory")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "symlink component", lstatRegular(r, "link/x"), "link is a symbolic link")
	if err := os.MkdirAll(filepath.Join(dir, "closed", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, filepath.Join(dir, "closed"), 0)
	if err := lstatRegular(r, "closed/sub/x"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("unreadable component: err = %v, want ErrPermission", err)
	}
}

func TestWriteFileAtFailures(t *testing.T) {
	r, dir := covRoot(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "through a link", writeFileAt(r, "link/f", []byte("x"), false, defaultPerms), "write f", "symbolic link")
	covWantErr(t, "missing dir", writeFileAt(r, "missing/f", []byte("x"), false, defaultPerms), "write f")

	covWriteFile(t, filepath.Join(dir, "taken"), "old")
	err := writeFileAt(r, "taken", []byte("new"), true, defaultPerms)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("exclusive over an existing file: err = %v, want ErrExist", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "taken")); string(data) != "old" {
		t.Errorf("the existing file was changed to %q", data)
	}

	if err := os.MkdirAll(filepath.Join(dir, "busy", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "rename over a directory", writeFileAt(r, "busy", []byte("x"), false, defaultPerms), "write busy")

	// No temporary file is left behind by any failure.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), forumfs.TempPrefix) {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
}

func TestWriteRelEscape(t *testing.T) {
	s := stNewStore(t)
	covWantErr(t, "escaping path", s.writeRel("../x", []byte("x"), false), "write forum file", "escapes the forum root")
	covWantErr(t, "empty path", s.writeRel("", []byte("x"), false), "escapes the forum root")
}

func TestLockLayoutErrors(t *testing.T) {
	s := stNewStore(t)
	locks := filepath.Join(s.base, forumfs.LocksDir)
	if err := os.RemoveAll(locks); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, locks, "not a directory")
	covWantErr(t, ".locks a file", newForumHandle(s.base, s.id).Lock(), "lock forum "+s.id)

	s2 := stNewStore(t)
	if err := os.Mkdir(newForumHandle(s2.base, s2.id).lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "lock file a directory", newForumHandle(s2.base, s2.id).Lock(), "lock forum "+s2.id)
}

func TestLockSweepFailures(t *testing.T) {
	// runs/ that is a file cannot be listed for the sweep.
	s := stNewStore(t)
	if err := os.RemoveAll(filepath.Join(s.dir, dirRuns)); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, filepath.Join(s.dir, dirRuns), "x")
	h := newForumHandle(s.base, s.id)
	covWantErr(t, "runs a file", h.Lock(), "sweep temporary files")
	if h.locked() {
		t.Error("a failed Lock left the lock held")
	}

	// A temporary entry that cannot be removed fails the sweep.
	s = stNewStore(t)
	tmp := filepath.Join(s.dir, dirRuns, forumfs.TempPrefix+"run-x")
	if err := os.MkdirAll(filepath.Join(tmp, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, tmp, 0o500)
	covWantErr(t, "stuck temp", newForumHandle(s.base, s.id).Lock(), "sweep temporary files")

	// The run sweep fails on a directory it cannot walk.
	s = stNewStore(t)
	closed := filepath.Join(s.root, dirLayers, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, closed, 0)
	covWantErr(t, "unwalkable run", s.Lock(), "sweep temporary files")

	// ... and on a temporary entry it cannot remove.
	s = stNewStore(t)
	ro := filepath.Join(s.root, dirLayers, "ro")
	if err := os.MkdirAll(filepath.Join(ro, forumfs.TempPrefix+"x", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	covChmod(t, ro, 0o500)
	covWantErr(t, "stuck run temp", s.Lock(), "sweep temporary files")
}

func TestUnlockReportsRemovalFailure(t *testing.T) {
	s := stNewStore(t)
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	covChmod(t, filepath.Join(s.base, forumfs.LocksDir), 0o500)
	covWantErr(t, "unlock", s.unlock(), "unlock forum "+s.id)
	if s.locked() {
		t.Error("the lock is still held after unlock")
	}
	// The lock was released: another handle takes it.
	h := newForumHandle(s.base, s.id)
	if err := h.Lock(); err != nil {
		t.Errorf("Lock after a failed unlock: %v", err)
	}
	h.Unlock()
}

func TestCleanupMarkerErrors(t *testing.T) {
	s := stNewStore(t)
	// A marker for a name that is not a run is skipped by MarkedRuns.
	covWriteFile(t, filepath.Join(s.base, dirCleanup, s.id+".x1."+cleanupAgents), "[]")
	if err := s.SetCleanup(cleanupAgents, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	runs, err := s.MarkedRuns(cleanupAgents)
	if err != nil || len(runs) != 1 || runs[0] != 1 {
		t.Errorf("MarkedRuns = %v, %v", runs, err)
	}

	// A marker that cannot be read is an error, not "absent".
	p := s.cleanupPath(cleanupAgents)
	covChmod(t, p, 0)
	_, ok, err := s.Cleanup(cleanupAgents)
	covWantErr(t, "unreadable marker", err, "read cleanup marker")
	if ok {
		t.Error("an unreadable marker reads as present")
	}

	cleanup := filepath.Join(s.base, dirCleanup)
	covChmod(t, cleanup, 0o500)
	covWantErr(t, "clear in read-only dir", s.ClearCleanup(cleanupAgents), "clear cleanup marker")
	covWantErr(t, "set in read-only dir", s.SetCleanup("other", []byte("x")), "write cleanup marker other")
	if err := os.Chmod(cleanup, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(cleanup); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "set without .cleanup", s.SetCleanup("other", []byte("x")), "write cleanup marker other")
}

func TestListStagedError(t *testing.T) {
	base := t.TempDir()
	covWriteFile(t, filepath.Join(base, dirCleanup), "x")
	_, err := listStaged(base)
	covWantErr(t, "listStaged", err, "list staged forums")
}

func TestRemoveStagedFailures(t *testing.T) {
	base := t.TempDir()
	id := uuid.NewString()
	cleanup := filepath.Join(base, dirCleanup)
	staged := filepath.Join(cleanup, id, "inner")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, filepath.Join(staged, "f"), "x")
	covChmod(t, staged, 0o500)
	covWantErr(t, "stuck staged root", removeStaged(base, id), "remove staged forum "+id)
	if err := os.Chmod(staged, 0o700); err != nil {
		t.Fatal(err)
	}

	// A marker that cannot be removed.
	if err := os.MkdirAll(filepath.Join(cleanup, id+".1.agents.json", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "stuck marker", removeStaged(base, id), "remove markers of forum "+id)
	if err := os.RemoveAll(filepath.Join(cleanup, id+".1.agents.json")); err != nil {
		t.Fatal(err)
	}
	if err := removeStaged(base, id); err != nil {
		t.Fatalf("removeStaged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cleanup, id)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staged root still there: %v", err)
	}
}

func TestRemoveFailures(t *testing.T) {
	// The cleanup folder cannot be created.
	s := stNewStore(t)
	if err := os.RemoveAll(filepath.Join(s.base, dirCleanup)); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, filepath.Join(s.base, dirCleanup), "x")
	covWantErr(t, ".cleanup a file", newForumHandle(s.base, s.id).Remove(), "remove forum "+s.id)

	// A stale staged copy that cannot be removed blocks the removal.
	s = stNewStore(t)
	stale := filepath.Join(s.base, dirCleanup, s.id, "inner")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	covWriteFile(t, filepath.Join(stale, "f"), "x")
	covChmod(t, stale, 0o500)
	covWantErr(t, "stuck stale copy", newForumHandle(s.base, s.id).Remove(), "remove forum "+s.id)
	if _, err := os.Stat(s.dir); err != nil {
		t.Errorf("forum gone after a failed removal: %v", err)
	}

	// The forum directory cannot be renamed out of a read-only base.
	s = stNewStore(t)
	h := newForumHandle(s.base, s.id)
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	covChmod(t, s.base, 0o500)
	covWantErr(t, "read-only base", h.Remove(), "remove forum "+s.id)
	if _, err := os.Stat(s.dir); err != nil {
		t.Errorf("forum gone after a failed removal: %v", err)
	}
}

func TestRemoveOfAlreadyStagedForum(t *testing.T) {
	// The forum directory is gone (staged and removed by an earlier run):
	// Remove finishes the markers.
	s := stNewStore(t)
	if err := s.SetCleanup(cleanupAgents, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	if err := newForumHandle(s.base, s.id).Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, _ := s.Cleanup(cleanupAgents); ok {
		t.Error("the marker survived Remove")
	}
}

func TestReadForumMetaErrors(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	covWriteFile(t, filepath.Join(s.dir, fileMeta), `{"owner":""}`)
	if _, err := f.ReadForumMeta(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("no owner: err = %v, want ErrCorrupt", err)
	}
	if err := os.Remove(filepath.Join(s.dir, fileMeta)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadForumMeta(); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	if err := f.WriteForumMeta(&ForumMeta{Owner: "a"}); err != nil {
		t.Fatalf("WriteForumMeta: %v", err)
	}
	if err := f.WriteForumMeta(&ForumMeta{Owner: "b"}); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second WriteForumMeta: err = %v, want ErrExist", err)
	}
	covChmod(t, s.dir, 0o500)
	covWantErr(t, "read-only forum dir", f.WriteForumConfig([]byte("{}")), "write the configuration of forum")
}

func TestReadForumConfigMissing(t *testing.T) {
	s := stNewStore(t)
	f := newForumHandle(s.base, s.id)
	if err := os.Remove(filepath.Join(s.dir, fileConfig)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.ReadForumConfig(); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadForumMeta(); err == nil {
		t.Error("forum dir missing: want an error")
	}
}
