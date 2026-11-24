// Agency
// License: MIT

package kbhost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/config"
)

func newStore(t *testing.T) *kb.Store {
	t.Helper()
	st, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

// alice is an agent with knowledge bases whose workspace is under dir, with
// the given shared entries.
func alice(dir string, shared ...config.SharedKnowledgeConfig) (*config.Config, *config.AgentConfig, string) {
	ws := filepath.Join(dir, "agents", "alice")
	cfg := &config.Config{}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Workspace: ws, Knowledge: &config.KnowledgeConfig{Enabled: true, Shared: shared}}}
	return cfg, &cfg.Agents.List[0], ws
}

func note(title string) kb.Frontmatter { return kb.Frontmatter{"type": "Note", "title": title} }

// Own bundles: bare names, under <workspace>/okf, created by a put, always
// writable, with the workspace's modes; names that are not one plain
// segment are unknown.
func TestResolver_OwnBundles(t *testing.T) {
	dir := t.TempDir()
	cfg, a, ws := alice(dir)
	cfg.Security.Permissions = config.DefaultPermissions()
	cfg.Security.Permissions.Agents = config.AreaPermissions{DirMode: "0750", FileMode: "0640"}
	r := New(cfg, a, ws)
	st := newStore(t)
	ctx := context.Background()

	if b, err := r.Bundles(ctx); err != nil || len(b) != 0 {
		t.Fatalf("Bundles before any = %v, %v", b, err)
	}
	if _, err := st.Get(ctx, r, "notes", "a"); !errors.Is(err, kb.ErrNotFound) {
		t.Fatalf("Get from a bundle not yet created = %v, want ErrNotFound", err)
	}
	res, err := st.Put(ctx, r, "notes", "guide/first", note("First"), "Alpha bravo.\n")
	if err != nil || !res.Created {
		t.Fatalf("Put = %+v, %v", res, err)
	}
	file := filepath.Join(ws, OwnDirName, "notes", "guide", "first.md")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("file mode = %o, want 0640", got)
	}
	if dirInfo, e := os.Stat(filepath.Join(ws, OwnDirName, "notes")); e != nil || dirInfo.Mode().Perm() != 0o750 {
		t.Errorf("bundle dir = %v, %v; want 0750", dirInfo, e)
	}
	for _, bad := range []string{"", ".hidden", "a/b", `a\b`, "../x", "x\x01"} {
		if _, e := st.Get(ctx, r, bad, "a"); !errors.Is(e, kb.ErrUnknownKB) {
			t.Errorf("Get from %q = %v, want ErrUnknownKB", bad, e)
		}
	}
	// A folder under okf/ that is a link, a dot folder or a file is no bundle.
	if e := os.Symlink(dir, filepath.Join(ws, OwnDirName, "linked")); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(ws, OwnDirName, ".git"), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(ws, OwnDirName, "file"), nil, 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := st.Get(ctx, r, "linked", "a"); e == nil {
		t.Error("Get followed a link out of okf/")
	}
	got, err := r.Bundles(ctx)
	if want := []kb.Info{{Name: "notes", Writable: true}}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Bundles = %v, %v; want %v", got, err, want)
	}
}

// Shared bundles: addressed as shared/<name>, read-only unless the entry
// allows writing, never created, and unknown while their folder is
// missing or not a directory.
func TestResolver_SharedBundles(t *testing.T) {
	dir := t.TempDir()
	team := filepath.Join(dir, "team")
	ref := filepath.Join(dir, "ref")
	gone := filepath.Join(dir, "gone")
	file := filepath.Join(dir, "file")
	for _, d := range []string{team, ref} {
		if err := os.Mkdir(d, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o770); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, a, ws := alice(dir,
		config.SharedKnowledgeConfig{Name: "team", Path: team + "/", Write: true},
		config.SharedKnowledgeConfig{Name: "ref", Path: ref},
		config.SharedKnowledgeConfig{Name: "gone", Path: gone, Write: true},
		config.SharedKnowledgeConfig{Name: "file", Path: file},
		// Refused on save; never resolved should one reach the resolver.
		config.SharedKnowledgeConfig{Name: "rel", Path: "rel"},
		config.SharedKnowledgeConfig{Name: "a/b", Path: team},
		config.SharedKnowledgeConfig{Name: "team", Path: ref},
	)
	r := New(cfg, a, ws)
	st := newStore(t)
	ctx := context.Background()

	if _, err := st.Put(ctx, r, "shared/team", "a", note("A"), "Shared text.\n"); err != nil {
		t.Fatalf("Put to a writable shared bundle = %v", err)
	}
	// A shared folder's own modes, files without execute bits: a
	// group-shared folder stays group-shared.
	if info, err := os.Stat(filepath.Join(team, "a.md")); err != nil || info.Mode().Perm() != 0o660 {
		t.Errorf("shared file = %v, %v; want 0660", info, err)
	}
	_, err := st.Put(ctx, r, "shared/ref", "a", note("A"), "")
	if !errors.Is(err, kb.ErrReadOnly) || !strings.Contains(err.Error(), `"shared/ref"`) {
		t.Errorf("Put to a read-only shared bundle = %v", err)
	}
	for _, name := range []string{"shared/gone", "shared/file", "shared/nope", "shared/rel", "shared/a/b", "shared/"} {
		_, e := st.Put(ctx, r, name, "a", note("A"), "")
		if !errors.Is(e, kb.ErrUnknownKB) {
			t.Errorf("Put to %q = %v, want ErrUnknownKB", name, e)
		}
	}
	if _, e := os.Stat(gone); !errors.Is(e, fs.ErrNotExist) {
		t.Errorf("a put created a shared folder: %v", e)
	}
	// "team" and "shared/team" are different bundles.
	if _, e := st.Get(ctx, r, "team", "a"); !errors.Is(e, kb.ErrNotFound) {
		t.Errorf("Get own team = %v, want ErrNotFound", e)
	}
	if _, e := st.Put(ctx, r, "team", "own", note("Own"), ""); e != nil {
		t.Fatal(e)
	}
	got, err := r.Bundles(ctx)
	want := []kb.Info{{Name: "team", Writable: true}, {Name: "shared/ref"}, {Name: "shared/team", Writable: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Bundles = %v, %v; want %v", got, err, want)
	}
	if paths := r.WatchPaths(); len(paths) != 5 || paths[0] != filepath.Join(ws, OwnDirName) || paths[1] != team {
		t.Errorf("WatchPaths = %v", paths)
	}
}

// A shared folder that appears later is used from then on.
func TestResolver_SharedFolderAppears(t *testing.T) {
	dir := t.TempDir()
	later := filepath.Join(dir, "later")
	cfg, a, ws := alice(dir, config.SharedKnowledgeConfig{Name: "later", Path: later})
	r := New(cfg, a, ws)
	if got, err := r.Bundles(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("Bundles = %v, %v", got, err)
	}
	if err := os.Mkdir(later, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Bundles(context.Background()); err != nil || len(got) != 1 || got[0].Name != "shared/later" {
		t.Errorf("Bundles = %v, %v", got, err)
	}
}

func TestResolver_BundlesUnreadable(t *testing.T) {
	dir := t.TempDir()
	cfg, a, ws := alice(dir)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	// okf is a file: it cannot be listed.
	if err := os.WriteFile(OwnDir(ws), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(cfg, a, ws)
	if _, err := r.Bundles(context.Background()); err == nil {
		t.Error("Bundles of an unreadable folder succeeded")
	}
	// The prompt still says something sensible.
	if got := PromptText(context.Background(), r); !strings.HasPrefix(got, "Bundles: none yet\n") {
		t.Errorf("PromptText = %q", got)
	}
}

// Without a config the store's modes are used (zero Modes).
func TestNew_NoConfig(t *testing.T) {
	r := New(nil, nil, "rel/ws")
	if r.modes != (fsmode.Modes{}) || !filepath.IsAbs(r.folder) || len(r.shared) != 0 {
		t.Errorf("resolver = %+v", r)
	}
}

func TestPromptText(t *testing.T) {
	const notice = "\n\nNote: Bundle names may conflict. Do not assume that \"mybundle\" and \"shared/mybundle\" are the same."
	dir := t.TempDir()
	for _, d := range []string{"ddd", "fff"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, a, ws := alice(dir,
		config.SharedKnowledgeConfig{Name: "fff", Path: filepath.Join(dir, "fff")},
		config.SharedKnowledgeConfig{Name: "ddd", Path: filepath.Join(dir, "ddd"), Write: true},
	)
	r := New(cfg, a, ws)
	ctx := context.Background()
	if got, want := PromptText(ctx, r), "Bundles: shared/ddd, shared/fff (read only)"+notice; got != want {
		t.Errorf("PromptText = %q, want %q", got, want)
	}
	for _, own := range []string{"b", "a"} {
		if err := os.MkdirAll(filepath.Join(ws, OwnDirName, own), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := PromptText(ctx, r), "Bundles: a, b, shared/ddd, shared/fff (read only)"+notice; got != want {
		t.Errorf("PromptText = %q, want %q", got, want)
	}
	_, none, nws := alice(t.TempDir())
	if got, want := PromptText(ctx, New(nil, none, nws)), "Bundles: none yet"+notice; got != want {
		t.Errorf("PromptText = %q, want %q", got, want)
	}
}

// The file tools never reach okf/ by default: it is in no default read or
// write folder and not among the folders always readable.
func TestOwnDirOutsideFileToolDefaults(t *testing.T) {
	d := config.DefaultConfig().Agents.Defaults
	for _, list := range [][]string{d.WorkspaceReadSubdirs, {d.WorkspaceWriteSubdir}, config.AlwaysReadableWorkspaceDirs} {
		for _, sub := range list {
			if filepath.Clean(sub) == OwnDirName || strings.HasPrefix(filepath.Clean(sub), OwnDirName+"/") {
				t.Errorf("%q is a default file-tool folder", sub)
			}
		}
	}
}
