package files

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"
)

// faultFs is a real fileSystem with failures injected by operation and path:
// fail returns the error an operation on a path fails with, or nil to let it
// through.
type faultFs struct {
	fileSystem
	fail func(op, path string) error
}

func (f *faultFs) check(op, path string) error {
	if f.fail == nil {
		return nil
	}
	return f.fail(op, path)
}

func (f *faultFs) ReadFile(p string) ([]byte, error) {
	if err := f.check("read", p); err != nil {
		return nil, err
	}
	return f.fileSystem.ReadFile(p)
}

func (f *faultFs) WriteFile(p string, d []byte) error {
	if err := f.check("write", p); err != nil {
		return err
	}
	return f.fileSystem.WriteFile(p, d)
}

func (f *faultFs) WriteFileMode(p string, d []byte, m os.FileMode) error {
	if err := f.check("write", p); err != nil {
		return err
	}
	return f.fileSystem.WriteFileMode(p, d, m)
}

func (f *faultFs) WriteFileExclMode(p string, d []byte, m os.FileMode) error {
	if err := f.check("excl", p); err != nil {
		return err
	}
	return f.fileSystem.WriteFileExclMode(p, d, m)
}

func (f *faultFs) ReadDir(p string) ([]os.DirEntry, error) {
	if err := f.check("readdir", p); err != nil {
		return nil, err
	}
	return f.fileSystem.ReadDir(p)
}

func (f *faultFs) Stat(p string) (os.FileInfo, error) {
	if err := f.check("stat", p); err != nil {
		return nil, err
	}
	return f.fileSystem.Stat(p)
}

func (f *faultFs) Remove(p string) error {
	if err := f.check("remove", p); err != nil {
		return err
	}
	return f.fileSystem.Remove(p)
}

func (f *faultFs) MkdirAll(p string) error {
	if err := f.check("mkdir", p); err != nil {
		return err
	}
	return f.fileSystem.MkdirAll(p)
}

var errInjected = errors.New("injected failure")

// failOn fails op on any path ending in suffix.
func failOn(op, suffix string) func(string, string) error {
	return func(o, p string) error {
		if o == op && strings.HasSuffix(p, suffix) {
			return errInjected
		}
		return nil
	}
}

func wantToolError(t *testing.T, what string, res *toolkit.Result, want string) {
	t.Helper()
	if !res.IsError || !strings.Contains(res.ForLLM, want) {
		t.Errorf("%s: want an error containing %q, got %v %q", what, want, res.IsError, res.ForLLM)
	}
}

func wantToolRefusal(t *testing.T, what string, res *toolkit.Result, want string) {
	t.Helper()
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || !strings.Contains(res.ForLLM, want) {
		t.Errorf("%s: want a refusal containing %q, got %v %q", what, want, res.IsError, res.ForLLM)
	}
}

// Tool names are released API.
func TestToolNames(t *testing.T) {
	ws := t.TempDir()
	for want, got := range map[string]string{
		"file_read_bytes":   NewReadFileTool(ws, true, 0, nil).Name(),
		"file_read_lines":   NewReadLinesTool(ws, true, 0, nil).Name(),
		"file_write":        NewWriteFileTool(ws, true).Name(),
		"file_list":         NewListDirTool(ws, true).Name(),
		"file_edit":         NewEditFileTool(ws, true).Name(),
		"file_append":       NewAppendFileTool(ws, true).Name(),
		"file_count":        NewCountFileTool(ws, true).Name(),
		"file_delete":       NewDeleteFileToolScoped(ws, true, "").Name(),
		"file_move":         NewMoveFileToolScoped(ws, true, "").Name(),
		"file_mkdir":        NewMkdirToolScoped(ws, true, "").Name(),
		"file_rmdir":        NewRmdirToolScoped(ws, true, "").Name(),
		"file_glob":         NewGlobTool(ws, true).Name(),
		"file_search_lines": NewSearchLinesTool(ws, true).Name(),
		"file_search_bytes": NewSearchBytesTool(ws, true).Name(),
		"view_image":        NewViewImageTool(ws, true).Name(),
	} {
		if got != want {
			t.Errorf("Name() = %q, want %q", got, want)
		}
	}
	if (&rangeEditTool{op: "move", unit: "lines"}).Description() != "" {
		t.Error("an unknown range op has no description")
	}
}

// The two read tools publish their own parameters: lines or bytes, and the
// byte tool's default length is its size limit.
func TestReadFileTool_Parameters(t *testing.T) {
	ws := t.TempDir()
	props := func(p map[string]any) map[string]any { return p["properties"].(map[string]any) }
	lines := props(NewReadLinesTool(ws, true, 0, nil).Parameters())
	if _, ok := lines["start_line"]; !ok {
		t.Error("read_lines has no start_line")
	}
	if _, ok := lines["offset"]; ok {
		t.Error("read_lines has offset")
	}
	byteTool := NewReadFileTool(ws, true, 1234, nil)
	b := props(byteTool.Parameters())
	if b["length"].(map[string]any)["default"] != int64(1234) {
		t.Errorf("length default = %v", b["length"])
	}
}

func TestReadFileTool_ArgumentsAndLimits(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "f.txt"), "alpha\nbeta\ngamma\n")
	ctx := context.Background()
	lines := NewReadLinesTool(ws, true, 0, nil)

	wantToolError(t, "bad start_line", lines.Execute(ctx, map[string]any{"path": "f.txt", "start_line": true}), "unsupported type bool for start_line")
	wantToolError(t, "bad line_count", lines.Execute(ctx, map[string]any{"path": "f.txt", "line_count": "x"}), "invalid integer format for line_count")
	wantToolError(t, "huge offset", NewReadFileTool(ws, true, 0, nil).Execute(ctx, map[string]any{"path": "f.txt", "offset": 1e20}), "offset value 1e+20 overflows int64")

	// start_line below 1 starts at 1; a line_count of 0 is the default.
	res := lines.Execute(ctx, map[string]any{"path": "f.txt", "start_line": -3, "line_count": 0})
	if res.IsError || !strings.HasPrefix(res.ForLLM, "1: alpha\n2: beta\n3: gamma\n") {
		t.Errorf("clamped read: %q", res.ForLLM)
	}
	// Past the end: no lines, and how many there are.
	res = lines.Execute(ctx, map[string]any{"path": "f.txt", "start_line": 9})
	if res.IsError || res.ForLLM != "[file: f.txt | no lines at start_line=9 (file has 3 line(s))]" {
		t.Errorf("past the end: %q", res.ForLLM)
	}
	// A byte limit smaller than the lines stops early and says so.
	small := NewReadLinesTool(ws, true, 24, nil)
	res = small.Execute(ctx, map[string]any{"path": "f.txt"})
	if res.IsError || !strings.HasPrefix(res.ForLLM, "1: alpha\n") || strings.Contains(res.ForLLM, "gamma") ||
		!strings.Contains(res.ForLLM, "Status: TRUNCATED at the byte limit — more lines remain") ||
		!strings.Contains(res.ForLLM, `file_read_lines(path="f.txt", start_line=2)`) {
		t.Errorf("byte-capped read: %q", res.ForLLM)
	}
}

// Without a known size, the byte status gives the range but no chunk count.
func TestByteReadStatus_UnknownSize(t *testing.T) {
	out := byteReadStatus("p", "p", 0, 10, 10, -1, false)
	if !strings.Contains(out, "Chunk returned: bytes 0-9\n") || strings.Contains(out, "Total size") || strings.Contains(out, "chunk 1") {
		t.Errorf("status: %q", out)
	}
}

// A recursive listing of a folder that cannot be read says so, like the
// one-level listing, instead of an empty list.
func TestListDir_RecursiveUnreadable(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "sessions", "s.jsonl"), "x")
	SetReadScopeSubdirs([]string{"files"})
	t.Cleanup(func() { SetReadScopeSubdirs(nil) })
	tool := NewListDirTool(ws, true)
	ctx := context.Background()

	res := tool.Execute(ctx, map[string]any{"path": "sessions", "recursive": true})
	wantToolError(t, "refused folder", res, "failed to read directory: read denied")
	if !toolkit.IsExpectedRefusal(res.Err) {
		t.Errorf("refused folder: not a refusal: %v", res.Err)
	}
	wantToolError(t, "missing folder", tool.Execute(ctx, map[string]any{"path": "files/none", "recursive": true}), "failed to read directory")
	wantToolError(t, "escape", tool.Execute(ctx, map[string]any{"path": "..", "recursive": true}), "failed to read directory")
}

// A recursive listing stops at its entry limit and says so.
func TestListDir_RecursiveTruncated(t *testing.T) {
	ws := t.TempDir()
	for i := range maxRecursiveListEntries + 5 {
		mustWrite(t, filepath.Join(ws, "d", strings.Repeat("a", 1+i/200)+string(rune('a'+i%26))+"-"+itoa(i)), "")
	}
	res := NewListDirTool(ws, true).Execute(context.Background(), map[string]any{"path": "d", "recursive": true})
	if res.IsError || strings.Count(res.ForLLM, "FILE: ") != maxRecursiveListEntries ||
		!strings.Contains(res.ForLLM, "[truncated at 1000 entries — list a subdirectory for more]") {
		t.Errorf("truncated listing: %d entries, tail %q", strings.Count(res.ForLLM, "FILE: "), res.ForLLM[max(0, len(res.ForLLM)-80):])
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// --- mkdir / rmdir ---

func TestMkdir_FailuresAndEscapes(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tool := NewMkdirToolScoped(ws, true, "")

	// Through a link out of the workspace: refused, nothing created outside.
	wantToolRefusal(t, "through a link", tool.Execute(ctx, map[string]any{"path": "out/new"}), "resolves outside the workspace")
	mustNotExist(t, filepath.Join(outside, "new"))

	// A folder that cannot be created.
	ro := filepath.Join(ws, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, ro)
	wantToolError(t, "read-only parent", tool.Execute(ctx, map[string]any{"path": "ro/new"}), "permission denied")
}

func TestRmdir_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("missing path", func(t *testing.T) {
		ws := t.TempDir()
		tool := NewRmdirToolScoped(ws, true, "")
		wantToolError(t, "no path", tool.Execute(ctx, map[string]any{"sure": true}), "path is required")
		wantToolError(t, "missing", tool.Execute(ctx, map[string]any{"path": "none", "sure": true}), "no such file")
	})

	t.Run("link in a read-only folder", func(t *testing.T) {
		ws := t.TempDir()
		ro := filepath.Join(ws, "ro")
		if err := os.Mkdir(ro, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".", filepath.Join(ro, "link")); err != nil {
			t.Fatal(err)
		}
		readOnlyDir(t, ro)
		res := NewRmdirToolScoped(ws, true, "").Execute(ctx, map[string]any{"path": "ro/link", "sure": true})
		wantToolError(t, "remove link", res, "access denied")
		if _, err := os.Lstat(filepath.Join(ro, "link")); err != nil {
			t.Errorf("link removed: %v", err)
		}
	})

	t.Run("unreadable folder", func(t *testing.T) {
		ws := t.TempDir()
		d := filepath.Join(ws, "d")
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if os.Geteuid() == 0 {
			t.Skip("permissions do not apply to root")
		}
		if err := os.Chmod(d, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
		res := NewRmdirToolScoped(ws, true, "").Execute(ctx, map[string]any{"path": "d", "sure": true})
		wantToolError(t, "list", res, "permission denied")
		mustExist(t, d)
	})

	t.Run("empty folder in a read-only folder", func(t *testing.T) {
		ws := t.TempDir()
		ro := filepath.Join(ws, "ro")
		if err := os.MkdirAll(filepath.Join(ro, "empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		readOnlyDir(t, ro)
		res := NewRmdirToolScoped(ws, true, "").Execute(ctx, map[string]any{"path": "ro/empty", "sure": true})
		wantToolError(t, "remove", res, "access denied")
		mustExist(t, filepath.Join(ro, "empty"))
	})

	t.Run("recursive with an unreadable subfolder", func(t *testing.T) {
		ws := t.TempDir()
		sub := filepath.Join(ws, "top", "sub")
		mustWrite(t, filepath.Join(sub, "f.txt"), "x")
		if os.Geteuid() == 0 {
			t.Skip("permissions do not apply to root")
		}
		if err := os.Chmod(sub, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		res := NewRmdirToolScoped(ws, true, "").Execute(ctx, map[string]any{"path": "top", "sure": true, "recursive": true})
		wantToolError(t, "walk", res, "permission denied")
		mustExist(t, filepath.Join(ws, "top"))
	})

	t.Run("recursive with a link named like a backup", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "top"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".", filepath.Join(ws, "top", "notes.md.0001")); err != nil {
			t.Fatal(err)
		}
		res := NewRmdirToolScoped(ws, true, "").Execute(ctx, map[string]any{"path": "top", "sure": true, "recursive": true})
		wantToolRefusal(t, "backup link", res, "the folder contains a backup file")
		mustExist(t, filepath.Join(ws, "top"))
	})

	t.Run("recursive removal that fails part way", func(t *testing.T) {
		ws := t.TempDir()
		mustWrite(t, filepath.Join(ws, "top", "a.txt"), "a")
		mustWrite(t, filepath.Join(ws, "top", "b.txt"), "b")
		tool := &RmdirTool{sysFs: &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("remove", "b.txt")}}
		res := tool.Execute(ctx, map[string]any{"path": "top", "sure": true, "recursive": true})
		wantToolError(t, "partial", res, "removed 1 of 3 entries in top, then failed on top/b.txt: injected failure; the rest is still there")
		mustNotExist(t, filepath.Join(ws, "top", "a.txt"))
		mustExist(t, filepath.Join(ws, "top", "b.txt"))
	})
}

// In an allowed folder, the folder's permissions decide: a read-only
// subfolder (or a file in one) inside the tree refuses the whole removal
// before anything is removed.
func TestRmdir_RecursiveReadOnlyInAllowedFolder(t *testing.T) {
	ctx := context.Background()
	for name, withFile := range map[string]bool{"file in a read-only folder": true, "empty read-only folder": false} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			allowed := t.TempDir()
			allowFolders(t, ws, allowed)
			top := filepath.Join(allowed, "top")
			sub := filepath.Join(top, "sub")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(top, "keep.txt"), "k")
			if withFile {
				mustWrite(t, filepath.Join(sub, "f.txt"), "x")
			}
			readOnlyDir(t, sub)
			res := NewRmdirToolScoped(ws, true, "files").Execute(ctx, map[string]any{"path": top, "sure": true, "recursive": true})
			wantToolRefusal(t, "rmdir", res, "is read-only for this app")
			mustExist(t, filepath.Join(top, "keep.txt"))
		})
	}
}

// --- move ---

func TestMove_ArgumentsAndFailures(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	tool := NewMoveFileToolScoped(ws, true, "")
	wantToolError(t, "no source", tool.Execute(ctx, map[string]any{"destination_path": "b"}), "source_path is required")
	wantToolError(t, "no destination", tool.Execute(ctx, map[string]any{"source_path": "a"}), "destination_path is required")

	// The copy is made but the source cannot be removed: both are named.
	mustWrite(t, filepath.Join(ws, "a.txt"), "a")
	faulty := &MoveFileTool{sysFs: &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("remove", "a.txt")}}
	res := faulty.Execute(ctx, map[string]any{"source_path": "a.txt", "destination_path": "b.txt"})
	wantToolError(t, "remove source", res, "copied to b.txt but failed to remove source a.txt: injected failure")
	mustRead(t, filepath.Join(ws, "b.txt"), "a")
	mustRead(t, filepath.Join(ws, "a.txt"), "a")
}

func TestMoveDir_Refusals(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) string {
		ws := t.TempDir()
		mustWrite(t, filepath.Join(ws, "src", "sub", "f.txt"), "f")
		mustWrite(t, filepath.Join(ws, "src", "g.txt"), "g")
		return ws
	}
	move := func(ws string, fsys fileSystem, args map[string]any) *toolkit.Result {
		if fsys == nil {
			fsys = buildWriteFs(ws, true, "", nil)
		}
		return (&MoveFileTool{sysFs: fsys}).Execute(ctx, args)
	}
	unchanged := func(t *testing.T, ws string) {
		t.Helper()
		mustRead(t, filepath.Join(ws, "src", "sub", "f.txt"), "f")
		mustRead(t, filepath.Join(ws, "src", "g.txt"), "g")
	}

	t.Run("destination is a file", func(t *testing.T) {
		ws := setup(t)
		mustWrite(t, filepath.Join(ws, "dst"), "x")
		wantToolError(t, "move", move(ws, nil, map[string]any{"source_path": "src", "destination_path": "dst"}), "destination is a file: dst")
		unchanged(t, ws)
	})
	t.Run("destination exists without overwrite", func(t *testing.T) {
		ws := setup(t)
		mustWrite(t, filepath.Join(ws, "dst", "z"), "z")
		wantToolError(t, "move", move(ws, nil, map[string]any{"source_path": "src", "destination_path": "dst"}), "destination already exists; pass overwrite=true")
		unchanged(t, ws)
	})
	t.Run("destination cannot be checked", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("stat", "dst")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}), "failed to stat destination: injected failure")
		unchanged(t, ws)
	})
	t.Run("source cannot be listed", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("readdir", "sub")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}), "injected failure")
		unchanged(t, ws)
		mustNotExist(t, filepath.Join(ws, "dst"))
	})
	t.Run("merge puts a file where a folder goes", func(t *testing.T) {
		ws := setup(t)
		mustWrite(t, filepath.Join(ws, "dst", "sub"), "file")
		wantToolError(t, "move", move(ws, nil, map[string]any{"source_path": "src", "destination_path": "dst", "overwrite": true}), "destination has a file where a folder goes: dst/sub")
		unchanged(t, ws)
	})
	t.Run("merge puts a folder where a file goes", func(t *testing.T) {
		ws := setup(t)
		if err := os.MkdirAll(filepath.Join(ws, "dst", "g.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
		wantToolError(t, "move", move(ws, nil, map[string]any{"source_path": "src", "destination_path": "dst", "overwrite": true}), "destination has a folder where a file goes: dst/g.txt")
		unchanged(t, ws)
	})
	t.Run("destination folder cannot be created", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("mkdir", "dst")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}),
			"folder move stopped at dst: injected failure. Moved 0 of 2 files to dst: none. Not moved, still in src: g.txt, sub/f.txt.")
		unchanged(t, ws)
	})
	t.Run("destination subfolder cannot be created", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("mkdir", "dst/sub")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}), "folder move stopped at dst/sub: injected failure")
		unchanged(t, ws)
	})
	t.Run("a source file cannot be removed", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("remove", "src/sub/f.txt")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}),
			"folder move stopped at src/sub/f.txt: copied but failed to remove the source: injected failure. Moved 1 of 2 files to dst: g.txt. Not moved, still in src: sub/f.txt.")
	})
	t.Run("an emptied source folder cannot be removed", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("remove", "src/sub")}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}),
			"folder move stopped at src/sub: injected failure. Moved 2 of 2 files to dst: g.txt, sub/f.txt. Not moved, still in src: none.")
		mustRead(t, filepath.Join(ws, "dst", "sub", "f.txt"), "f")
	})
	t.Run("the source folder cannot be removed", func(t *testing.T) {
		ws := setup(t)
		fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: func(op, p string) error {
			if op == "remove" && p == "src" {
				return errInjected
			}
			return nil
		}}
		wantToolError(t, "move", move(ws, fsys, map[string]any{"source_path": "src", "destination_path": "dst"}), "folder move stopped at src: injected failure")
		mustRead(t, filepath.Join(ws, "dst", "g.txt"), "g")
	})
}

// Moving a folder out of an allowed folder whose subfolder is read-only is
// refused before anything moves.
func TestMoveDir_ReadOnlySubfolderInAllowedFolder(t *testing.T) {
	ws := t.TempDir()
	allowed := t.TempDir()
	allowFolders(t, ws, allowed)
	src := filepath.Join(allowed, "src")
	mustWrite(t, filepath.Join(src, "sub", "f.txt"), "f")
	mustWrite(t, filepath.Join(src, "g.txt"), "g")
	readOnlyDir(t, filepath.Join(src, "sub"))
	res := NewMoveFileToolScoped(ws, true, "files").Execute(context.Background(),
		map[string]any{"source_path": src, "destination_path": filepath.Join(allowed, "dst")})
	wantToolRefusal(t, "move", res, "is read-only for this app")
	mustRead(t, filepath.Join(src, "g.txt"), "g")
	mustNotExist(t, filepath.Join(allowed, "dst"))
}

// --- delete ---

func TestDelete_Failures(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	tool := NewDeleteFileToolScoped(ws, true, "")
	wantToolError(t, "no path", tool.Execute(ctx, map[string]any{"sure": true}), "path is required")
	wantToolError(t, "missing", tool.Execute(ctx, map[string]any{"path": "none.txt", "sure": true}), "no such file")

	ro := filepath.Join(ws, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("none", filepath.Join(ro, "link")); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, ro)
	wantToolError(t, "link in a read-only folder", tool.Execute(ctx, map[string]any{"path": "ro/link", "sure": true}), "access denied")
	if _, err := os.Lstat(filepath.Join(ro, "link")); err != nil {
		t.Errorf("link removed: %v", err)
	}
}

// --- backups ---

func TestBackupExistingFile_Failures(t *testing.T) {
	ws := t.TempDir()
	base := buildWriteFs(ws, true, "", nil)
	mustWrite(t, filepath.Join(ws, "f.txt"), "f")
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := backupExistingFile(base, "dir"); err == nil || !strings.Contains(err.Error(), "backup: target is a directory: dir") {
		t.Errorf("directory: %v", err)
	}
	for _, tc := range []struct {
		name string
		fail func(string, string) error
		want string
	}{
		{"siblings", failOn("readdir", "."), "backup: failed to scan siblings: injected failure"},
		{"target", failOn("read", "f.txt"), "backup: failed to read target: injected failure"},
		{"write", failOn("excl", "f.txt.0001"), "backup: failed to write f.txt.0001: injected failure"},
		{"stat", failOn("stat", "f.txt"), "backup: failed to stat target: injected failure"},
		{"always taken", func(op, _ string) error {
			if op == "excl" {
				return fs.ErrExist
			}
			return nil
		}, `backup: exhausted 100 attempts to find unused suffix for "f.txt"`},
	} {
		_, err := backupExistingFile(&faultFs{fileSystem: base, fail: tc.fail}, "f.txt")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	// With .9998 the last free suffix, a taken .9999 exhausts the range.
	mustWrite(t, filepath.Join(ws, "f.txt.9998"), "old")
	taken := &faultFs{fileSystem: base, fail: func(op, _ string) error {
		if op == "excl" {
			return fs.ErrExist
		}
		return nil
	}}
	if _, err := backupExistingFile(taken, "f.txt"); err == nil || !strings.Contains(err.Error(), `backup: suffix range exhausted for "f.txt" (max .9999 in use)`) {
		t.Errorf("range: %v", err)
	}
	// With .9999 in use there is no next suffix.
	mustWrite(t, filepath.Join(ws, "f.txt.9999"), "old")
	if _, err := backupExistingFile(base, "f.txt"); err == nil {
		t.Error("backup past .9999 made")
	}
	mustRead(t, filepath.Join(ws, "f.txt"), "f")
}

// A failed backup or write leaves the file as it was, for every editing tool.
func TestEditTools_BackupAndWriteFailures(t *testing.T) {
	ctx := context.Background()
	type tool interface {
		Execute(context.Context, map[string]any) *toolkit.Result
	}
	cases := []struct {
		name string
		make func(fileSystem) tool
		args map[string]any
	}{
		{"edit", func(f fileSystem) tool { return &EditFileTool{sysFs: f} }, map[string]any{"path": "f.txt", "old_text": "one", "new_text": "uno", "backup": true}},
		{"append", func(f fileSystem) tool { return &AppendFileTool{sysFs: f} }, map[string]any{"path": "f.txt", "content": "more", "backup": true}},
		{"edit_lines", func(f fileSystem) tool { return &rangeEditTool{sysFs: f, op: "edit", unit: "lines"} }, map[string]any{"path": "f.txt", "start": 1, "replace": "uno", "backup": true}},
	}
	for _, tc := range cases {
		for _, fault := range []struct {
			name string
			fail func(string, string) error
		}{
			{"backup", failOn("excl", "f.txt.0001")},
			{"write", failOn("write", "f.txt")},
		} {
			t.Run(tc.name+"/"+fault.name, func(t *testing.T) {
				ws := t.TempDir()
				mustWrite(t, filepath.Join(ws, "f.txt"), "one\n")
				fsys := &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: fault.fail}
				wantToolError(t, tc.name, tc.make(fsys).Execute(ctx, tc.args), "injected failure")
				mustRead(t, filepath.Join(ws, "f.txt"), "one\n")
			})
		}
	}
}

// --- copy ---

func TestCopy_WriteFailure(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a.txt"), "a")
	tool := &CopyFileTool{sysFs: &faultFs{fileSystem: buildWriteFs(ws, true, "", nil), fail: failOn("write", "b.txt")}}
	wantToolError(t, "copy", tool.Execute(context.Background(), map[string]any{"source_path": "a.txt", "destination_path": "b.txt"}), "injected failure")
	mustNotExist(t, filepath.Join(ws, "b.txt"))
}

// --- range edits ---

func TestRangeEdit_ArgumentErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		op, unit, body string
		args           map[string]any
		want           string
	}{
		{"insert", "lines", "a\n", map[string]any{"after_line": 0}, "text is required (non-empty)"},
		{"insert", "lines", "a\n", map[string]any{"after_line": "x", "text": "t"}, "invalid integer format for after_line"},
		{"insert", "lines", "a\n", map[string]any{"after_line": 5, "text": "t"}, "after_line 5 out of range (file has 1 line(s); use 0..1)"},
		{"edit", "lines", "", map[string]any{"start": 1, "replace": "r"}, "file has no lines"},
		{"edit", "lines", "a\n", map[string]any{"start": 1.5, "replace": "r"}, "start must be an integer"},
		{"edit", "lines", "a\n", map[string]any{"start": 1, "end": true, "replace": "r"}, `end must be a number or "end" for end of file`},
		{"edit", "lines", "a\n", map[string]any{"start": 3, "replace": "r"}, "start 3 is past end of file (file has 1 line(s); use file_insert_lines to add)"},
		{"edit", "lines", "a\nb\n", map[string]any{"start": 2, "end": 1, "replace": "r"}, "end 1 is before start 2"},
		{"insert", "bytes", "abc", map[string]any{"at_offset": 0}, "text is required (non-empty)"},
		{"insert", "bytes", "abc", map[string]any{"at_offset": []int{1}, "text": "t"}, "unsupported type []int for at_offset"},
		{"insert", "bytes", "abc", map[string]any{"at_offset": 4, "text": "t"}, "at_offset 4 out of range (file is 3 bytes; use 0..3)"},
		{"edit", "bytes", "", map[string]any{"start": 0, "replace": "r"}, "file is empty"},
		{"edit", "bytes", "abc", map[string]any{"start": "x", "replace": "r"}, "invalid integer format for start"},
		{"edit", "bytes", "abc", map[string]any{"start": -1, "replace": "r"}, "start must be >= 0"},
		{"edit", "bytes", "abc", map[string]any{"start": 3, "replace": "r"}, "start 3 is at/past end of file (file is 3 bytes; use file_insert_bytes to add)"},
		{"edit", "bytes", "abc", map[string]any{"start": 1, "end": "x", "replace": "r"}, `end must be a number or "end" for end of file`},
		{"edit", "bytes", "abc", map[string]any{"start": 2, "end": 1, "replace": "r"}, "end 1 is before start 2"},
		{"edit", "bytes", "abc", map[string]any{"start": 1}, "replace must be non-empty; to remove bytes use file_delete_bytes"},
	}
	for _, tc := range cases {
		dir, p := writeTemp(t, tc.body)
		args := map[string]any{"path": p, "backup": false}
		for k, v := range tc.args {
			args[k] = v
		}
		res := rangeTool(tc.op, tc.unit, dir).Execute(ctx, args)
		wantToolError(t, tc.op+"_"+tc.unit+" "+tc.want, res, tc.want)
		if got := readBack(t, p); got != tc.body {
			t.Errorf("%s_%s changed the file: %q", tc.op, tc.unit, got)
		}
	}
	dir, _ := writeTemp(t, "a")
	wantToolError(t, "no path", rangeTool("edit", "lines", dir).Execute(ctx, map[string]any{}), "path is required")
}

// A line edit at start 0 starts at line 1; deleting every line leaves an
// empty file; display shows what was removed.
func TestRangeEdit_EdgesAndDisplay(t *testing.T) {
	ctx := context.Background()
	dir, p := writeTemp(t, "a\nb\n")
	res := rangeTool("edit", "lines", dir).Execute(ctx, map[string]any{"path": p, "start": 0, "replace": "A", "backup": false, "display": true})
	if res.IsError || res.ForLLM != "Replaced lines 1-1 (1→1 line(s)) in "+p || !strings.Contains(res.ForUser, "A") {
		t.Errorf("edit: %+v", res)
	}
	res = rangeTool("delete", "lines", dir).Execute(ctx, map[string]any{"path": p, "start": 1, "end": "end", "backup": false})
	if res.IsError {
		t.Fatalf("delete: %s", res.ForLLM)
	}
	if got := readBack(t, p); got != "" {
		t.Errorf("after deleting every line: %q", got)
	}
}

// --- count ---

// errReadFs opens files whose reads fail.
type errReadFs struct{ fileSystem }

type errFile struct{ fs.File }

func (errFile) Read([]byte) (int, error) { return 0, errInjected }
func (errFile) Close() error             { return nil }

func (e *errReadFs) Open(string) (fs.File, error) { return errFile{}, nil }

func TestCount_ReadFailure(t *testing.T) {
	tool := &CountFileTool{sysFs: &errReadFs{}}
	wantToolError(t, "count", tool.Execute(context.Background(), map[string]any{"path": "f.txt"}), `failed to read "f.txt": injected failure`)
}

// --- patterns and read policy ---

func TestCompilePatterns_SkipsInvalid(t *testing.T) {
	got := compilePatterns([]string{"^/ok/", "([", "^/also/"})
	if len(got) != 2 || got[0].String() != "^/ok/" || got[1].String() != "^/also/" {
		t.Errorf("compilePatterns = %v", got)
	}
}

func TestReadPolicy_NilConfig(t *testing.T) {
	if restrict, allow := ReadPolicy(nil); restrict || allow != nil {
		t.Errorf("ReadPolicy(nil) = %v %v", restrict, allow)
	}
}

// --- search ---

func TestSearch_ArgumentsAndByteMode(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	long := strings.Repeat("x", 300)
	mustWrite(t, filepath.Join(ws, "d", "a.txt"), "needle one\n"+long+"needle"+long+"\nneedle two\n")
	mustWrite(t, filepath.Join(ws, "d", "b.txt"), "nothing here\n")
	lines := NewSearchLinesTool(ws, true)
	byteTool := NewSearchBytesTool(ws, true)

	wantToolError(t, "no query", lines.Execute(ctx, map[string]any{"query": "  "}), "query is required")
	wantToolError(t, "bad max_results", lines.Execute(ctx, map[string]any{"query": "a", "max_results": true}), "unsupported type bool for max_results")
	wantToolError(t, "bad context", lines.Execute(ctx, map[string]any{"query": "a", "path": "d", "context": "x"}), "invalid integer format for context")

	// A long matching line is cut for display.
	res := lines.Execute(ctx, map[string]any{"query": "needle", "path": "d/a.txt"})
	if res.IsError || !strings.Contains(res.ForLLM, "d/a.txt:2: "+strings.Repeat("x", 240)+"…") {
		t.Errorf("long line: %s", res.ForLLM)
	}

	// Byte mode on one file: offsets, the snippet cut, and a limit of 0 is the
	// maximum.
	res = byteTool.Execute(ctx, map[string]any{"query": "NEEDLE", "path": "d/a.txt", "max_results": 0})
	if res.IsError || !strings.Contains(res.ForLLM, "3 match(es) for \"NEEDLE\" in d/a.txt (byte offsets):") ||
		!strings.Contains(res.ForLLM, "d/a.txt: bytes 0-6: needle one") ||
		!strings.Contains(res.ForLLM, "d/a.txt: bytes 311-317: needle"+strings.Repeat("x", 80)) {
		t.Errorf("byte search: %s", res.ForLLM)
	}
	// Truncated at max_results.
	res = byteTool.Execute(ctx, map[string]any{"query": "needle", "path": "d", "max_results": 2})
	if res.IsError || !strings.Contains(res.ForLLM, "[Showing the first 2 — narrow the query or search a subdirectory for more.]") {
		t.Errorf("truncated byte search: %s", res.ForLLM)
	}
	// No match.
	res = byteTool.Execute(ctx, map[string]any{"query": "absent", "path": "d"})
	if res.IsError || res.ForLLM != `No matches for "absent" in d.` {
		t.Errorf("no match: %s", res.ForLLM)
	}
	// Outside the workspace: refused.
	res = byteTool.Execute(ctx, map[string]any{"query": "root", "path": "/etc"})
	if !res.IsError {
		t.Errorf("byte search outside the workspace: %s", res.ForLLM)
	}
	// A cancelled search stops.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	wantToolError(t, "cancelled", byteTool.Execute(cctx, map[string]any{"query": "needle", "path": "d"}), "search stopped: context canceled")
}

// --- view image ---

func TestViewImage_Edges(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	tool := NewViewImageTool(ws, true)

	wantToolError(t, "no path", tool.Execute(ctx, map[string]any{"path": " "}), "path is required")

	mustWrite(t, filepath.Join(ws, "doc.pdf"), "%PDF-1.4\n%âãÏÓ\n1 0 obj\n")
	wantToolError(t, "pdf", tool.Execute(ctx, map[string]any{"path": "doc.pdf"}), `"doc.pdf" is not an image (detected application/pdf)`)

	// An image that cannot be decoded here is passed through as it is.
	corrupt := append(append([]byte{}, makePNG(t, 2, 2)[:16]...), []byte("garbage")...)
	mustWriteBytes(t, filepath.Join(ws, "bad.png"), corrupt)
	res := tool.Execute(ctx, map[string]any{"path": "bad.png"})
	if res.IsError || res.ForLLM != "[viewing image bad.png — 0×0 (passed through)]" || len(res.Images) != 1 ||
		!strings.HasPrefix(res.Images[0], "data:image/png;base64,") {
		t.Errorf("corrupt image: %+v", res)
	}

	// A very thin image keeps at least one pixel on its short edge.
	for name, size := range map[string][2]int{"wide.png": {5000, 1}, "tall.png": {1, 5000}} {
		mustWriteBytes(t, filepath.Join(ws, name), makePNG(t, size[0], size[1]))
		res := tool.Execute(ctx, map[string]any{"path": name})
		img := decodeDataURL(t, res.Images[0])
		if b := img.Bounds(); res.IsError || min(b.Dx(), b.Dy()) != 1 || !strings.Contains(res.ForLLM, "downscaled from") {
			t.Errorf("%s: %s %v", name, res.ForLLM, img.Bounds())
		}
	}
}
