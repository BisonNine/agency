// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package layout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.bison9.com/agency/config"
)

// newConfig returns the default config with CLAW_HOME at a fresh temp dir.
func newConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "claw-home")
	t.Setenv("CLAW_HOME", home)
	return config.DefaultConfig(), home
}

func assertPrivateDir(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("%s mode = %04o, want 0700", dir, fi.Mode().Perm())
	}
}

func TestPrepareFreshStartCreatesDirectories(t *testing.T) {
	cfg, home := newConfig(t)

	Prepare(cfg)

	assertPrivateDir(t, home)
	for _, dir := range []string{"internal", "bin", "cli", "skills", "cards", "common"} {
		assertPrivateDir(t, filepath.Join(home, dir))
	}
}

// A second start finds everything in place and changes nothing.
func TestPrepareIsIdempotent(t *testing.T) {
	cfg, home := newConfig(t)

	Prepare(cfg)
	stateFile := filepath.Join(home, "internal", "state.json")
	if err := os.WriteFile(stateFile, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	Prepare(cfg)

	b, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read %s: %v", stateFile, err)
	}
	if string(b) != "kept" {
		t.Errorf("state.json = %q after a second start, want it untouched", b)
	}
	for _, dir := range []string{"internal", "bin", "cli", "skills", "cards", "common"} {
		assertPrivateDir(t, filepath.Join(home, dir))
	}
}

// agents.common_dir is created instead of <CLAW_HOME>/common.
func TestPrepareCommonDirOverride(t *testing.T) {
	cfg, home := newConfig(t)
	custom := filepath.Join(t.TempDir(), "shared")
	cfg.Agents.CommonDir = custom

	Prepare(cfg)

	assertPrivateDir(t, custom)
	if _, err := os.Lstat(filepath.Join(home, "common")); !os.IsNotExist(err) {
		t.Errorf("%s/common created although agents.common_dir is set (err %v)", home, err)
	}
}

// Without a data directory there is nothing to prepare.
func TestPrepareNoDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	Prepare(&config.Config{})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Prepare without a data directory created %v", entries)
	}
}

// A directory that cannot be created is skipped; the others are still made.
func TestPrepareContinuesPastAFailedDirectory(t *testing.T) {
	cfg, home := newConfig(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// A dangling link: it does not stat, and MkdirAll cannot replace it.
	skills := filepath.Join(home, "skills")
	if err := os.Symlink(filepath.Join(home, "missing"), skills); err != nil {
		t.Fatal(err)
	}

	Prepare(cfg)

	if fi, err := os.Lstat(skills); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("skills = %v, %v; want the link left alone", fi, err)
	}
	for _, dir := range []string{"internal", "bin", "cli", "cards", "common"} {
		assertPrivateDir(t, filepath.Join(home, dir))
	}
}

// A mode that cannot be set leaves the directory created and the start going.
func TestPrepareContinuesPastAFailedChmod(t *testing.T) {
	cfg, home := newConfig(t)
	saved := chmod
	t.Cleanup(func() { chmod = saved })
	chmod = func(name string, mode os.FileMode) error {
		if filepath.Base(name) == "cli" {
			return os.ErrPermission
		}
		return saved(name, mode)
	}

	Prepare(cfg)

	if fi, err := os.Stat(filepath.Join(home, "cli")); err != nil || !fi.IsDir() {
		t.Errorf("cli = %v, %v; want it created", fi, err)
	}
	for _, dir := range []string{"internal", "bin", "skills", "cards", "common"} {
		assertPrivateDir(t, filepath.Join(home, dir))
	}
}
