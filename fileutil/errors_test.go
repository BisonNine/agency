// Agency
// License: MIT

//go:build unix

package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

// readOnlyDir makes a 0500 directory, restored to 0700 at cleanup so
// t.TempDir can remove it.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestMkdirAllWith_PerFolderModes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b")
	modeFor := func(dir string) os.FileMode {
		if filepath.Base(dir) == "a" {
			return 0o750
		}
		return 0o700
	}
	if err := MkdirAllWith(target, modeFor); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]os.FileMode{filepath.Join(root, "a"): 0o750, target: 0o700} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", dir, fi.Mode().Perm(), want)
		}
	}
	// An existing folder is left as it is.
	if err := MkdirAllWith(target, func(string) os.FileMode { return 0o777 }); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("existing folder mode changed to %04o", fi.Mode().Perm())
	}
}

func TestMkdirAllExact_Errors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllExact(file, 0o700); !errors.Is(err, os.ErrExist) {
		t.Errorf("path is a file: err = %v, want ErrExist", err)
	}
	if err := MkdirAllExact(filepath.Join(file, "sub"), 0o700); err == nil {
		t.Error("parent is a file: want error")
	}

	skipIfRoot(t)
	if err := MkdirAllExact(filepath.Join(readOnlyDir(t), "sub"), 0o700); err == nil {
		t.Error("read-only parent: want error")
	}
}

func TestWriteFileExact_Errors(t *testing.T) {
	if err := WriteFileExact(filepath.Join(t.TempDir(), "missing", "f"), []byte("x"), 0o600); err == nil {
		t.Error("missing parent: want error")
	}
}

func TestWriteFileExact_ExistingFileGetsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileExact(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", fi.Mode().Perm())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Errorf("content = %q, want new", data)
	}
}

func TestWriteFileAtomic_Errors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFileAtomic(filepath.Join(file, "sub", "x"), []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "failed to create directory") {
		t.Errorf("parent under a file: err = %v", err)
	}

	// The target is a non-empty directory: the rename fails and the temp
	// file is removed.
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err = os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = WriteFileAtomic(target, []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "failed to rename temp file") {
		t.Errorf("target is a dir: err = %v", err)
	}
	entries := dirEntries(t, dir)
	if len(entries) != 1 {
		t.Errorf("dir holds %d entries after failed write, want only the target", len(entries))
	}

	skipIfRoot(t)
	err = WriteFileAtomic(filepath.Join(readOnlyDir(t), "x"), []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "failed to create temp file") {
		t.Errorf("read-only dir: err = %v", err)
	}
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if !Writable(dir) {
		t.Errorf("Writable(%s) = false for a fresh temp dir", dir)
	}
	if Writable(filepath.Join(dir, "missing")) {
		t.Error("Writable on a missing path = true")
	}
	skipIfRoot(t)
	if Writable(readOnlyDir(t)) {
		t.Error("Writable on a 0500 dir = true")
	}
}

// dirEntries lists dir, failing the test when it cannot be read.
func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
