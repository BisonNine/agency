package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"
)

// wantRefusal fails unless err is an expected refusal whose text holds want.
func wantRefusal(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil || !toolkit.IsExpectedRefusal(err) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: want a refusal containing %q, got %v", what, want, err)
	}
}

// wantErrContains fails unless err is non-nil and its text holds want.
func wantErrContains(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: want an error containing %q, got %v", what, want, err)
	}
}

// readOnlyDir makes dir read-only for the test and restores it at cleanup.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})
}

// wantMode fails unless path has the permission bits want.
func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode %v, want %v", path, got, want)
	}
}

// noTempFiles fails when an atomic write left a temporary file in dir.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// --- hostFs (an unrestricted agent) ---

func TestHostFs_WriteAndRemove(t *testing.T) {
	dir := t.TempDir()
	h := &hostFs{}

	p := filepath.Join(dir, "a", "b", "f.txt")
	if err := h.WriteFile(p, []byte("one")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRead(t, p, "one")
	if err := h.WriteFileMode(p, []byte("two"), 0o600); err != nil {
		t.Fatalf("WriteFileMode: %v", err)
	}
	mustRead(t, p, "two")
	wantMode(t, p, 0o600)
	if err := h.MkdirAll(filepath.Join(dir, "x", "y")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustExist(t, filepath.Join(dir, "x", "y"))

	// A parent that is a file cannot be created.
	blocked := filepath.Join(dir, "a", "b", "f.txt", "g.txt")
	wantErrContains(t, "WriteFile under a file", h.WriteFile(blocked, nil), "file already exists")
	wantErrContains(t, "WriteFileMode under a file", h.WriteFileMode(blocked, nil, 0o600), "file already exists")
	wantErrContains(t, "WriteFileExclMode under a file", h.WriteFileExclMode(blocked, nil, 0o600), "file already exists")

	// Exclusive create: a new file gets the mode, an existing one is left as is.
	excl := filepath.Join(dir, "excl.txt")
	if err := h.WriteFileExclMode(excl, []byte("new"), 0o640); err != nil {
		t.Fatalf("WriteFileExclMode: %v", err)
	}
	wantMode(t, excl, 0o640)
	if err := h.WriteFileExclMode(excl, []byte("again"), 0o640); !errors.Is(err, fs.ErrExist) {
		t.Errorf("excl over an existing file: %v", err)
	}
	mustRead(t, excl, "new")

	// Remove: missing, not empty, and done.
	wantErrContains(t, "remove missing", h.Remove(filepath.Join(dir, "missing")), "failed to delete file: file not found")
	err := h.Remove(filepath.Join(dir, "a"))
	wantErrContains(t, "remove a non-empty folder", err, "failed to delete file: ")
	if err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "access denied")) {
		t.Errorf("remove a non-empty folder: %v", err)
	}
	if err := h.Remove(excl); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mustNotExist(t, excl)

	if info, err := h.Lstat(dir); err != nil || !info.IsDir() {
		t.Errorf("Lstat: %v %v", info, err)
	}
}

func TestHostFs_RemoveReadOnlyFolder(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "x")
	readOnlyDir(t, dir)
	wantErrContains(t, "remove in a read-only folder", (&hostFs{}).Remove(p), "failed to delete file: access denied")
	mustExist(t, p)
}

// A folder is not a regular file: reading it is refused.
func TestHostFs_ReadFolderRefused(t *testing.T) {
	_, err := (&hostFs{}).ReadFile(t.TempDir())
	wantRefusal(t, "read a folder", err, "is not a regular file")
}

// --- regularOpen and discardPartial ---

func TestRegularOpen_RefusesAnotherFile(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	mustWrite(t, a, "a")
	mustWrite(t, b, "b")
	other, err := os.Lstat(b)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, oerr := regularOpen(f, a, other); oerr == nil || !strings.Contains(oerr.Error(), "changed while it was being opened") {
		t.Errorf("a different file: %v", oerr)
	}
	// The refused file was closed.
	if cerr := f.Close(); !errors.Is(cerr, os.ErrClosed) {
		t.Errorf("file left open: %v", cerr)
	}

	// A file that cannot be stat-ed (closed) is an error, not a refusal.
	g, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := regularOpen(g, a, nil); err == nil || !strings.Contains(err.Error(), "failed to open file") {
		t.Errorf("closed file: %v", err)
	}
}

func TestDiscardPartial_RemovesThePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "partial")
	mustWrite(t, p, "half")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil { // closed again below: that failure is only logged
		t.Fatal(err)
	}
	var removed []string
	discardPartial(f, func(path string) error {
		removed = append(removed, path)
		return errors.New("busy") // a cleanup failure is only logged
	}, p)
	if len(removed) != 1 || removed[0] != p {
		t.Errorf("removed = %v", removed)
	}
	discardPartial(nil, os.Remove, p)
	mustNotExist(t, p)
}

// --- sandboxFs (a confined agent) ---

func TestSandboxFs_NoWorkspace(t *testing.T) {
	_, err := (&sandboxFs{}).ReadFile("a.txt")
	wantErrContains(t, "empty workspace", err, "workspace is not defined")
	_, err = (&sandboxFs{workspace: filepath.Join(t.TempDir(), "gone")}).ReadFile("a.txt")
	wantErrContains(t, "missing workspace", err, "failed to open workspace")
	if (&sandboxFs{}).isRoot("x") {
		t.Error("isRoot with no workspace")
	}
}

func TestSandboxFs_PathEscapes(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	sb := &sandboxFs{workspace: ws}

	for _, p := range []string{"../x.txt", filepath.Join(outside, "secret.txt"), "/etc/passwd"} {
		if _, err := sb.ReadFile(p); err == nil || !strings.Contains(err.Error(), "path escapes workspace") {
			t.Errorf("read %s: %v", p, err)
		}
		if err := sb.WriteFileExclMode(p, []byte("x"), 0o600); err == nil {
			t.Errorf("excl write %s allowed", p)
		}
	}

	via := "out/secret.txt"
	_, err := sb.ReadFile(via)
	wantRefusal(t, "read through a link", err, "resolves outside the workspace")
	wantRefusal(t, "write through a link", sb.WriteFileMode(via, []byte("x"), 0o600), "resolves outside the workspace")
	wantRefusal(t, "excl write through a link", sb.WriteFileExclMode("out/new.txt", []byte("x"), 0o600), "resolves outside the workspace")
	wantRefusal(t, "remove through a link", sb.Remove(via), "resolves outside the workspace")
	_, err = sb.Stat(via)
	wantRefusal(t, "stat through a link", err, "resolves outside the workspace")
	mustRead(t, filepath.Join(outside, "secret.txt"), "secret")
	mustNotExist(t, filepath.Join(outside, "new.txt"))

	// Lstat names the link itself, inside the workspace.
	if info, lerr := sb.Lstat("out"); lerr != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Lstat link: %v %v", info, lerr)
	}

	// A refusal names what the sandbox stands for.
	named := &sandboxFs{workspace: ws, outside: "the folders the agent can read"}
	_, err = named.ReadFile(via)
	wantRefusal(t, "named outside", err, "resolves outside the folders the agent can read")
}

func TestSandboxFs_WriteFailures(t *testing.T) {
	ws := t.TempDir()
	sb := &sandboxFs{workspace: ws}
	mustWrite(t, filepath.Join(ws, "file"), "x")

	// A parent that is a file.
	wantErrContains(t, "write under a file", sb.WriteFileMode("file/sub/a.txt", nil, 0o600), "failed to create parent directories")
	wantErrContains(t, "excl under a file", sb.WriteFileExclMode("file/sub/a.txt", nil, 0o600), "failed to create parent directories")
	if err := sb.MkdirAll("file"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("MkdirAll over a file: %v", err)
	}

	// Replacing a folder fails at the rename; the temporary file is removed.
	if err := os.MkdirAll(filepath.Join(ws, "d", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws, "d", "dir", "keep"), "k")
	wantErrContains(t, "write over a folder", sb.WriteFileMode("d/dir", []byte("x"), 0o600), "failed to rename temp file over target")
	noTempFiles(t, filepath.Join(ws, "d"))

	// Exclusive create over an existing file fails and keeps it.
	if err := sb.WriteFileExclMode("file", []byte("y"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Errorf("excl over a file: %v", err)
	}
	mustRead(t, filepath.Join(ws, "file"), "x")

	// A new file in a new folder gets its mode.
	if err := sb.WriteFileExclMode("n/new.txt", []byte("new"), 0o640); err != nil {
		t.Fatalf("excl: %v", err)
	}
	wantMode(t, filepath.Join(ws, "n", "new.txt"), 0o640)
}

func TestSandboxFs_ReadOnlyFolder(t *testing.T) {
	ws := t.TempDir()
	sb := &sandboxFs{workspace: ws}
	ro := filepath.Join(ws, "ro")
	mustWrite(t, filepath.Join(ro, "f.txt"), "x")
	readOnlyDir(t, ro)

	wantErrContains(t, "write", sb.WriteFileMode("ro/g.txt", []byte("y"), 0o600), "failed to open temp file")
	wantErrContains(t, "remove", sb.Remove("ro/f.txt"), "failed to delete file: access denied")
	mustExist(t, filepath.Join(ro, "f.txt"))
	mustNotExist(t, filepath.Join(ro, "g.txt"))
}

func TestSandboxFs_Remove(t *testing.T) {
	ws := t.TempDir()
	sb := &sandboxFs{workspace: ws}
	mustWrite(t, filepath.Join(ws, "d", "f.txt"), "x")
	wantErrContains(t, "missing", sb.Remove("nope.txt"), "failed to delete file: file not found")
	err := sb.Remove("d")
	wantErrContains(t, "non-empty", err, "failed to delete file: ")
	if err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "access denied")) {
		t.Errorf("non-empty folder: %v", err)
	}
	if err := sb.Remove("d/f.txt"); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(ws, "d", "f.txt"))
}

// --- whitelistFs (allow-listed host paths) ---

func TestWhitelistFs_HostOperations(t *testing.T) {
	ws := t.TempDir()
	host := t.TempDir()
	w := &whitelistFs{
		sandbox:  &sandboxFs{workspace: ws},
		patterns: []*regexp.Regexp{regexp.MustCompile("^" + regexp.QuoteMeta(host) + "/")},
	}
	p := filepath.Join(host, "sub", "f.txt")
	if err := w.WriteFileMode(p, []byte("one"), 0o600); err != nil {
		t.Fatalf("WriteFileMode: %v", err)
	}
	mustRead(t, p, "one")
	excl := filepath.Join(host, "e.txt")
	if err := w.WriteFileExclMode(excl, []byte("e"), 0o600); err != nil {
		t.Fatalf("WriteFileExclMode: %v", err)
	}
	if err := w.MkdirAll(filepath.Join(host, "m")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustExist(t, filepath.Join(host, "m"))
	if info, err := w.Lstat(excl); err != nil || info.Size() != 1 {
		t.Errorf("Lstat: %v %v", info, err)
	}
	if err := w.Remove(excl); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mustNotExist(t, excl)
	if w.isRoot(filepath.Join(host, "sub")) {
		t.Error("an allow-listed folder is not a root")
	}

	// Paths not on the list go to the sandbox.
	if err := w.WriteFileMode("w.txt", []byte("w"), 0o600); err != nil {
		t.Fatalf("sandbox WriteFileMode: %v", err)
	}
	if err := w.WriteFileExclMode("x.txt", []byte("x"), 0o600); err != nil {
		t.Fatalf("sandbox WriteFileExclMode: %v", err)
	}
	if err := w.MkdirAll("dir"); err != nil {
		t.Fatalf("sandbox MkdirAll: %v", err)
	}
	if info, err := w.Lstat("w.txt"); err != nil || info.Size() != 1 {
		t.Errorf("sandbox Lstat: %v %v", info, err)
	}
	if err := w.Remove("x.txt"); err != nil {
		t.Fatalf("sandbox Remove: %v", err)
	}
	if !w.isRoot(ws) {
		t.Error("the workspace is the sandbox's root")
	}
	if err := w.Remove("../escape"); err == nil {
		t.Error("sandbox remove outside the workspace allowed")
	}
}

// A link in an allow-listed folder that leads elsewhere is refused for every
// read, so it cannot reach a file the agent may not read.
func TestWhitelistFs_LinkOutRefused(t *testing.T) {
	ws := t.TempDir()
	host := t.TempDir()
	secret := t.TempDir()
	mustWrite(t, filepath.Join(secret, "key"), "k")
	if err := os.Symlink(secret, filepath.Join(host, "link")); err != nil {
		t.Fatal(err)
	}
	w := &whitelistFs{
		sandbox:  &sandboxFs{workspace: ws},
		patterns: []*regexp.Regexp{regexp.MustCompile("^" + regexp.QuoteMeta(host) + "/")},
	}
	dirLink := filepath.Join(host, "link")
	fileLink := filepath.Join(dirLink, "key")
	_, err := w.ReadFile(fileLink)
	wantRefusal(t, "ReadFile", err, "resolves outside the folders the agent can read")
	_, err = w.Stat(fileLink)
	wantRefusal(t, "Stat", err, "resolves outside the folders the agent can read")
	_, err = w.Open(fileLink)
	wantRefusal(t, "Open", err, "resolves outside the folders the agent can read")
	_, err = w.ReadDir(dirLink + "/")
	wantRefusal(t, "ReadDir", err, "resolves outside the folders the agent can read")
}

// --- readScopedFs (reads confined to files/, skills/, ...) ---

func TestReadScopedFs_WritesPassThrough(t *testing.T) {
	ws := t.TempDir()
	r := newReadScopedFs(&sandboxFs{workspace: ws}, ws, []string{"files"}, nil)
	if err := r.WriteFileMode("other/a.txt", []byte("a"), 0o600); err != nil {
		t.Fatalf("WriteFileMode: %v", err)
	}
	if err := r.WriteFileExclMode("other/b.txt", []byte("b"), 0o600); err != nil {
		t.Fatalf("WriteFileExclMode: %v", err)
	}
	if err := r.MkdirAll("other/c"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustRead(t, filepath.Join(ws, "other", "a.txt"), "a")
	mustRead(t, filepath.Join(ws, "other", "b.txt"), "b")
	if err := r.Remove("other/b.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mustNotExist(t, filepath.Join(ws, "other", "b.txt"))
	if !r.isRoot(ws) || r.isRoot(filepath.Join(ws, "other")) {
		t.Error("isRoot is the layer below's")
	}

	// Reads outside the scope are refused, with the scope named.
	_, err := r.ReadFile("other/a.txt")
	wantRefusal(t, "read outside the scope", err, "read denied: the agent can only read files/")
}

// Lstat of a link in the read scope that points outside it names the link
// (the link itself is in scope); following it is refused.
func TestReadScopedFs_LstatLinkOut(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "sessions", "s.jsonl"), "secret")
	if err := os.MkdirAll(filepath.Join(ws, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../sessions/s.jsonl", filepath.Join(ws, "files", "link")); err != nil {
		t.Fatal(err)
	}
	r := newReadScopedFs(&sandboxFs{workspace: ws}, ws, []string{"files"}, nil)
	if info, err := r.Lstat("files/link"); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Lstat: %v %v", info, err)
	}
	_, err := r.Stat("files/link")
	wantRefusal(t, "Stat through the link", err, "resolves outside the folders the agent can read")
}

// A link to an allow-listed host path is read there.
func TestReadScopedFs_LinkToAllowListedPath(t *testing.T) {
	ws := t.TempDir()
	host := t.TempDir()
	mustWrite(t, filepath.Join(host, "doc.md"), "doc")
	if err := os.MkdirAll(filepath.Join(ws, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A relative link inside the workspace that leaves it (and so reaches the
	// host path) is the sandbox's to refuse; the read scope does not.
	link := filepath.Join(ws, "files", "doc")
	if err := os.Symlink(filepath.Join(host, "doc.md"), link); err != nil {
		t.Fatal(err)
	}
	patterns := []*regexp.Regexp{regexp.MustCompile("^" + regexp.QuoteMeta(host) + "/")}
	r := newReadScopedFs(&whitelistFs{sandbox: &sandboxFs{workspace: ws}, patterns: patterns}, ws, []string{"files"}, patterns)
	// The absolute link leaves the workspace: the sandbox below refuses it.
	if _, err := r.ReadFile("files/doc"); err == nil {
		t.Error("absolute link out of the workspace read")
	}
	// The host path itself is read.
	if got, err := r.ReadFile(filepath.Join(host, "doc.md")); err != nil || string(got) != "doc" {
		t.Errorf("host read: %q %v", got, err)
	}
}

// A workspace-relative path that cannot be resolved (the workspace is gone)
// is an error, not a read.
func TestReadScopedFs_WorkspaceGone(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "gone")
	r := newReadScopedFs(&sandboxFs{workspace: ws}, ws, []string{"files"}, nil)
	_, err := r.ReadFile("files/a.txt")
	wantErrContains(t, "read", err, "failed to open workspace")
}

// A link loop in the read scope is refused, not followed forever.
func TestReadScopedFs_LinkLoop(t *testing.T) {
	ws := t.TempDir()
	files := filepath.Join(ws, "files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(files, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(files, "b")); err != nil {
		t.Fatal(err)
	}
	r := newReadScopedFs(&sandboxFs{workspace: ws}, ws, []string{"files"}, nil)
	if _, err := r.ReadFile("files/a"); err == nil {
		t.Error("link loop read")
	}
}

// --- writeScopedFs (writes confined to files/) ---

func TestWriteScopedFs_Refusals(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws, "skills", "s.md"), "skill")
	w := &writeScopedFs{inner: &sandboxFs{workspace: ws}, workspace: ws, writeRoot: filepath.Join(ws, "files")}

	wantRefusal(t, "MkdirAll outside", w.MkdirAll("skills/new"), "write denied: outside")
	wantRefusal(t, "WriteFileMode outside", w.WriteFileMode("skills/s.md", []byte("x"), 0o600), "write denied: outside")
	wantRefusal(t, "WriteFileExclMode outside", w.WriteFileExclMode("skills/t.md", []byte("x"), 0o600), "write denied: outside")
	wantRefusal(t, "Remove outside", w.Remove("skills/s.md"), "write denied: outside")
	mustRead(t, filepath.Join(ws, "skills", "s.md"), "skill")
	mustNotExist(t, filepath.Join(ws, "skills", "t.md"))

	// Inside files/ every write works, reads pass through.
	if err := w.WriteFileMode("files/a.txt", []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFileExclMode("files/b.txt", []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.MkdirAll("files/c"); err != nil {
		t.Fatal(err)
	}
	f, err := w.Open("files/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != "a" {
		t.Errorf("Open read %q", got)
	}

	// The write folder and the workspace are roots; a folder in it is not.
	if !w.isRoot("files") || !w.isRoot(filepath.Join(ws, "files")) || !w.isRoot(ws) || w.isRoot("files/c") {
		t.Error("isRoot")
	}

	// A link loop inside the write folder is refused, not followed forever.
	if err := os.Symlink("y", filepath.Join(ws, "files", "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("x", filepath.Join(ws, "files", "y")); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, "write through a loop", w.WriteFileMode("files/x/z.txt", []byte("z"), 0o600), "resolves outside")

	// A link to an absolute path is refused.
	if err := os.Symlink(filepath.Join(ws, "skills"), filepath.Join(ws, "files", "abs")); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, "write through an absolute link", w.WriteFileMode("files/abs/s.md", []byte("x"), 0o600), "resolves outside")
	mustRead(t, filepath.Join(ws, "skills", "s.md"), "skill")
}

// When the write folder itself is a link out of the workspace, every write
// into it is refused.
func TestWriteScopedFs_WriteRootLinkOut(t *testing.T) {
	ws := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(ws, "files")); err != nil {
		t.Fatal(err)
	}
	w := &writeScopedFs{inner: &sandboxFs{workspace: ws}, workspace: ws, writeRoot: filepath.Join(ws, "files")}
	wantRefusal(t, "write", w.WriteFileMode("files/a.txt", []byte("a"), 0o600), "resolves outside")
	mustNotExist(t, filepath.Join(elsewhere, "a.txt"))
}

// A workspace that cannot be opened is an error for a write check.
func TestWriteScopedFs_WorkspaceGone(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "gone")
	w := &writeScopedFs{inner: &sandboxFs{workspace: ws}, workspace: ws, writeRoot: filepath.Join(ws, "files")}
	wantErrContains(t, "write", w.WriteFileMode("files/a.txt", nil, 0o600), "failed to open workspace")
}

// --- ReadFileTool on a file that cannot seek ---

// noSeekFs serves one in-memory file that does not implement io.Seeker.
type noSeekFs struct {
	fileSystem
	data []byte
}

type noSeekFile struct {
	r    io.Reader
	size int64
}

func (f *noSeekFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *noSeekFile) Close() error               { return nil }
func (f *noSeekFile) Stat() (fs.FileInfo, error) { return sizeInfo(f.size), nil }

type sizeInfo int64

func (s sizeInfo) Name() string       { return "stream" }
func (s sizeInfo) Size() int64        { return int64(s) }
func (s sizeInfo) Mode() fs.FileMode  { return 0o644 }
func (s sizeInfo) ModTime() time.Time { return time.Time{} }
func (s sizeInfo) IsDir() bool        { return false }
func (s sizeInfo) Sys() any           { return nil }

func (n *noSeekFs) Open(string) (fs.File, error) {
	return &noSeekFile{r: bytes.NewReader(n.data), size: int64(len(n.data))}, nil
}

func TestReadFileTool_NonSeekable(t *testing.T) {
	data := []byte(strings.Repeat("a", 600) + "TAIL")
	tool := &ReadFileTool{sysFs: &noSeekFs{data: data}, maxSize: MaxReadFileSize}
	ctx := context.Background()

	// An offset inside the sniffed bytes cannot be reached.
	res := tool.Execute(ctx, map[string]any{"path": "stream", "offset": 10})
	if !res.IsError || !strings.Contains(res.ForLLM, "non-seekable file") {
		t.Errorf("offset in sniff: %s", res.ForLLM)
	}
	// An offset past them is reached by reading forward.
	res = tool.Execute(ctx, map[string]any{"path": "stream", "offset": 600})
	if res.IsError || !strings.HasPrefix(res.ForLLM, "TAIL") {
		t.Errorf("offset past sniff: %s", res.ForLLM)
	}
	// An offset past the end cannot be reached.
	res = tool.Execute(ctx, map[string]any{"path": "stream", "offset": 5000})
	if !res.IsError || !strings.Contains(res.ForLLM, "failed to advance to offset 5000") {
		t.Errorf("offset past end: %s", res.ForLLM)
	}
}

// --- allowedFs (an agent's allowed folders) ---

// A path that names an allowed folder but leads out of every folder the
// agent may use, through a link, is refused for every operation of a
// confined agent; nothing outside is touched.
func TestAllowedFs_LinkOutRefused(t *testing.T) {
	ws := t.TempDir()
	allowed := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(allowed, "out")); err != nil {
		t.Fatal(err)
	}
	allowFolders(t, ws, allowed)
	a := withAllowedFolders(ws, true, &sandboxFs{workspace: ws})

	file := filepath.Join(allowed, "out", "secret.txt")
	newFile := filepath.Join(allowed, "out", "new.txt")
	const want = "resolves outside the allowed folders"
	wantRefusal(t, "WriteFile", a.WriteFile(newFile, []byte("x")), want)
	wantRefusal(t, "WriteFileMode", a.WriteFileMode(newFile, []byte("x"), 0o600), want)
	wantRefusal(t, "WriteFileExclMode", a.WriteFileExclMode(newFile, []byte("x"), 0o600), want)
	wantRefusal(t, "Remove", a.Remove(file), want)
	wantRefusal(t, "MkdirAll", a.MkdirAll(filepath.Join(allowed, "out", "d")), want)
	_, err := a.Lstat(file)
	wantRefusal(t, "Lstat", err, want)
	_, err = a.ReadFile(file)
	wantRefusal(t, "ReadFile", err, want)
	if isRoot(a, filepath.Join(allowed, "out", "d")) {
		t.Error("isRoot of a refused path")
	}
	mustRead(t, filepath.Join(outside, "secret.txt"), "secret")
	mustNotExist(t, filepath.Join(outside, "new.txt"))
	mustNotExist(t, filepath.Join(outside, "d"))

	// Unconfined, the same path is served where it leads.
	open := withAllowedFolders(ws, false, &hostFs{})
	if got, err := open.ReadFile(file); err != nil || string(got) != "secret" {
		t.Errorf("unconfined read: %q %v", got, err)
	}
}

// In an allowed folder, writes and their failures: the folder's own
// permissions decide, and a permission error is a short refusal naming the
// path.
func TestAllowedFs_WritesInFolder(t *testing.T) {
	ws := t.TempDir()
	allowed := t.TempDir()
	allowFolders(t, ws, allowed)
	a := withAllowedFolders(ws, true, &sandboxFs{workspace: ws})

	p := filepath.Join(allowed, "f.txt")
	if err := a.WriteFileMode(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteFileExclMode(filepath.Join(allowed, "g.txt"), []byte("g"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := a.Lstat(p); err != nil || info.Size() != 3 {
		t.Errorf("Lstat: %v %v", info, err)
	}
	if !isRoot(a, allowed) || isRoot(a, p) {
		t.Error("isRoot: the allowed folder is a root, a file in it is not")
	}

	ro := filepath.Join(allowed, "ro")
	mustWrite(t, filepath.Join(ro, "keep.txt"), "k")
	readOnlyDir(t, ro)
	const want = "is read-only for this app"
	wantRefusal(t, "WriteFileMode", a.WriteFileMode(filepath.Join(ro, "x"), []byte("x"), 0o600), want)
	wantRefusal(t, "WriteFileExclMode", a.WriteFileExclMode(filepath.Join(ro, "x"), []byte("x"), 0o600), want)
	wantRefusal(t, "Remove", a.Remove(filepath.Join(ro, "keep.txt")), want)
	wantRefusal(t, "MkdirAll", a.MkdirAll(filepath.Join(ro, "d")), want)
	mustExist(t, filepath.Join(ro, "keep.txt"))
}

// Setting folders for no workspace does nothing; an empty list clears them.
func TestSetAllowedFolders_NoWorkspace(t *testing.T) {
	SetAllowedFoldersForWorkspace("", []string{"/x"})
	if got := allowedFoldersForWorkspace(""); got != nil {
		t.Errorf("folders for no workspace: %v", got)
	}
	ws := t.TempDir()
	SetAllowedFoldersForWorkspace(ws, []string{"/x"})
	SetAllowedFoldersForWorkspace(ws, nil)
	if got := allowedFoldersForWorkspace(ws); got != nil {
		t.Errorf("cleared folders: %v", got)
	}
}

// writableInFolder looks at the nearest existing folder.
func TestWritableInFolder(t *testing.T) {
	dir := t.TempDir()
	if !writableInFolder(filepath.Join(dir, "a", "b", "c.txt")) {
		t.Error("missing path under a writable folder")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, ro)
	if writableInFolder(ro) || writableInFolder(filepath.Join(ro, "new", "x")) {
		t.Error("a read-only folder is not writable")
	}
}
