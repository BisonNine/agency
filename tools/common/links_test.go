package common

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

// at is path as a location confined to its own folder.
func at(path string) location {
	return location{base: filepath.Dir(path), rel: filepath.Base(path), shown: path, area: "its folder"}
}

// linkFixture is a workspace and a common folder, each with a file, and a
// folder outside both holding a secret.
type linkFixture struct {
	ws, common, outside string
	defs                []toolspec.ToolDefinition
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	f := &linkFixture{ws: t.TempDir(), common: t.TempDir(), outside: t.TempDir()}
	mustWriteFile(t, filepath.Join(f.outside, "secret"), "secret")
	mustWriteFile(t, filepath.Join(f.common, "doc.md"), "from common")
	mustWriteFile(t, filepath.Join(f.ws, "files", "notes.md"), "notes")
	f.defs = GlobalProvider.RegisterTools(newDeps(t, f.common, f.ws, nil))
	return f
}

func (f *linkFixture) link(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

func (f *linkFixture) run(t *testing.T, tool string, args map[string]any) *toolspec.Result {
	t.Helper()
	return call(t, findTool(t, f.defs, tool), args)
}

// secretIntact fails unless the file outside is untouched.
func (f *linkFixture) secretIntact(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(f.outside, "secret"))
	if err != nil || string(got) != "secret" {
		t.Errorf("file outside changed: %q %v", got, err)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantLinkRefusal(t *testing.T, what string, res *toolspec.Result, want string) {
	t.Helper()
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || res.ForLLM != want {
		t.Errorf("%s: want refusal %q, got %v %q", what, want, res.IsError, res.ForLLM)
	}
}

// common_put never reads a file outside the workspace through a link: not
// the file itself, not through a linked folder.
func TestPut_LinkOutOfWorkspaceRefused(t *testing.T) {
	f := newLinkFixture(t)
	f.link(t, filepath.Join(f.outside, "secret"), filepath.Join(f.ws, "files", "link"))
	f.link(t, f.outside, filepath.Join(f.ws, "files", "dir"))
	f.link(t, "../../"+filepath.Base(f.outside)+"/secret", filepath.Join(f.ws, "files", "rel")) // relative, still outside

	wantLinkRefusal(t, "file link", f.run(t, "put", map[string]any{"path": "files/link", "as": "leak"}),
		"common_put: access denied: files/link resolves outside the workspace")
	wantLinkRefusal(t, "folder link", f.run(t, "put", map[string]any{"path": "files/dir/secret", "as": "leak"}),
		"common_put: access denied: files/dir/secret resolves outside the workspace")
	if res := f.run(t, "put", map[string]any{"path": "files/rel", "as": "leak"}); !res.IsError {
		t.Errorf("relative link out: %s", res.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(f.common, "leak")); !os.IsNotExist(err) {
		t.Errorf("a file outside the workspace reached common: %v", err)
	}
}

// common_get never writes outside the workspace through a link: an existing
// destination link, or a linked parent folder.
func TestGet_LinkOutOfWorkspaceRefused(t *testing.T) {
	f := newLinkFixture(t)
	f.link(t, filepath.Join(f.outside, "secret"), filepath.Join(f.ws, "files", "link"))
	f.link(t, f.outside, filepath.Join(f.ws, "files", "dir"))

	wantLinkRefusal(t, "destination link", f.run(t, "get", map[string]any{"name": "doc.md", "as": "link"}),
		"common_get: access denied: files/link resolves outside the workspace")
	wantLinkRefusal(t, "parent link", f.run(t, "get", map[string]any{"name": "doc.md", "as": "dir/secret"}),
		"common_get: access denied: files/dir/secret resolves outside the workspace")
	wantLinkRefusal(t, "new file under a parent link", f.run(t, "get", map[string]any{"name": "doc.md", "as": "dir/new/x.md"}),
		"common_get: access denied: files/dir/new/x.md resolves outside the workspace")
	f.secretIntact(t)
	if _, err := os.Stat(filepath.Join(f.outside, "new")); !os.IsNotExist(err) {
		t.Errorf("a folder was created outside the workspace: %v", err)
	}
}

// A link placed in the common folder lets neither tool reach outside it.
func TestCommonFolderLinkRefused(t *testing.T) {
	f := newLinkFixture(t)
	f.link(t, filepath.Join(f.outside, "secret"), filepath.Join(f.common, "link"))
	f.link(t, f.outside, filepath.Join(f.common, "dir"))

	wantLinkRefusal(t, "get through a link", f.run(t, "get", map[string]any{"name": "link", "as": "got.md"}),
		"common_get: access denied: link resolves outside the common folder")
	wantLinkRefusal(t, "get through a folder link", f.run(t, "get", map[string]any{"name": "dir/secret", "as": "got.md"}),
		"common_get: access denied: dir/secret resolves outside the common folder")
	if _, err := os.Stat(filepath.Join(f.ws, "files", "got.md")); !os.IsNotExist(err) {
		t.Errorf("a file outside common reached the workspace: %v", err)
	}
	wantLinkRefusal(t, "put over a link", f.run(t, "put", map[string]any{"path": "files/notes.md", "as": "link"}),
		"common_put: access denied: link resolves outside the common folder")
	wantLinkRefusal(t, "put through a folder link", f.run(t, "put", map[string]any{"path": "files/notes.md", "as": "dir/secret"}),
		"common_put: access denied: dir/secret resolves outside the common folder")
	f.secretIntact(t)
}

// Links that stay inside their folder keep working, both ways.
func TestLinksInsideKeepWorking(t *testing.T) {
	f := newLinkFixture(t)
	mustWriteFile(t, filepath.Join(f.ws, "tasks", "t.json"), "task")
	f.link(t, "../tasks/t.json", filepath.Join(f.ws, "files", "task-link"))
	f.link(t, "../tasks", filepath.Join(f.ws, "files", "tasks"))
	f.link(t, "doc.md", filepath.Join(f.common, "alias.md"))

	if res := f.run(t, "put", map[string]any{"path": "files/task-link", "as": "task.json"}); res.IsError {
		t.Fatalf("put through an inside link: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(filepath.Join(f.common, "task.json")); err != nil || string(got) != "task" {
		t.Errorf("common copy: %q %v", got, err)
	}
	if res := f.run(t, "get", map[string]any{"name": "alias.md", "as": "alias.md"}); res.IsError {
		t.Fatalf("get through a common link: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(filepath.Join(f.ws, "files", "alias.md")); err != nil || !strings.Contains(string(got), "from common") {
		t.Errorf("workspace copy: %q %v", got, err)
	}
	res := f.run(t, "get", map[string]any{"name": "doc.md", "as": "tasks/from-common.md"})
	if res.IsError {
		t.Fatalf("get into a linked folder inside the workspace: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(filepath.Join(f.ws, "tasks", "from-common.md")); err != nil || string(got) != "from common" {
		t.Errorf("copy through the inside folder link: %q %v", got, err)
	}
}

// copyFile's folder failures: a confining folder that is gone, a file where
// a folder goes, and a folder that cannot be created.
func TestCopyFile_FolderFailures(t *testing.T) {
	modes := config.Modes{Dir: 0o755, File: 0o644}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	mustWriteFile(t, src, "s")
	gone := location{base: filepath.Join(dir, "gone"), rel: "x", shown: "x", area: "the folder"}
	if err := copyFile(gone, at(filepath.Join(dir, "out")), modes); err == nil {
		t.Error("source folder gone")
	}
	if err := copyFile(at(src), gone, modes); err == nil {
		t.Error("destination folder gone")
	}
	mustWriteFile(t, filepath.Join(dir, "a", "file"), "f")
	under := location{base: dir, rel: filepath.Join("a", "file", "x"), shown: "a/file/x", area: "the folder"}
	if err := copyFile(at(src), under, modes); !errors.Is(err, fs.ErrExist) {
		t.Errorf("file where a folder goes: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ro, 0o700); err != nil {
			t.Errorf("restore %s: %v", ro, err)
		}
	})
	deep := location{base: ro, rel: filepath.Join("n1", "n2", "x"), shown: "n1/n2/x", area: "the folder"}
	if err := copyFile(at(src), deep, modes); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("read-only folder: %v", err)
	}
}
