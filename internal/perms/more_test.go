//go:build !windows

package perms

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/config"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
}

// fileIn returns a path below a regular file, which no call can create or
// stat as a directory.
func fileIn(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	return filepath.Join(file, "below")
}

func TestModesForZeroPolicy(t *testing.T) {
	assert.Equal(t, config.Modes{Dir: config.DefaultDirMode, File: config.DefaultFileMode}, Policy{}.ModesFor("/anything"))
}

func TestModesForConfig(t *testing.T) {
	home := t.TempDir()
	cfg := operatorConfig(t, home)
	assert.Equal(t, fs.FileMode(0o750), ModesFor(cfg, home).Dir, "the data directory takes the root folder mode")
	assert.Equal(t, config.Modes{Dir: 0o770, File: 0o660}, ModesFor(cfg, filepath.Join(home, "agents", "alice", "notes.md")))
	assert.Equal(t, PolicyFor(cfg).ModesFor(filepath.Join(cfg.CardsPath(), "card.json")), CardModes(cfg))
	assert.Equal(t, config.Modes{Dir: 0o710, File: 0o604}, CardModes(cfg))
}

func TestPolicyWriteFileParentFails(t *testing.T) {
	home := t.TempDir()
	p := PolicyFor(operatorConfig(t, home))
	require.Error(t, p.WriteFile(filepath.Join(fileIn(t), "x.txt"), []byte("x")))
}

func TestSetPolicy(t *testing.T) {
	saved, savedSecrets := CurrentPolicy(), SecretModes()
	t.Cleanup(func() {
		SetPolicy(saved)
		SetSecretModes(savedSecrets)
	})
	p := PolicyFor(operatorConfig(t, t.TempDir()))
	p.Secrets = config.Modes{Dir: 0o750, File: 0o640}
	SetPolicy(p)
	assert.Equal(t, p, CurrentPolicy())
	assert.Equal(t, config.Modes{Dir: 0o750, File: 0o640}, SecretModes())
}

func TestRunningExecutableMissing(t *testing.T) {
	old := executable
	t.Cleanup(func() { executable = old })
	executable = func() (string, error) { return filepath.Join(t.TempDir(), "gone"), nil }
	assert.Nil(t, runningExecutable())
}

// A file its owner cannot read is still set, through a write-only open.
func TestEnforceWriteOnlyFile(t *testing.T) {
	skipIfRoot(t)
	noExecutable(t)
	home := t.TempDir()
	cfg := operatorConfig(t, home)
	p := place(t, home, "skills/demo/SKILL.md", 0o200)
	var rec recorder
	require.NoError(t, Enforce(home, PolicyFor(cfg), rec.logger()))
	assert.Equal(t, fs.FileMode(0o604), modeOf(t, p))
}

// A file that cannot be opened at all is left as it is, with a WARN.
func TestEnforceUnopenableFileWarns(t *testing.T) {
	skipIfRoot(t)
	noExecutable(t)
	home := t.TempDir()
	cfg := operatorConfig(t, home)
	p := place(t, home, "skills/demo/SKILL.md", 0o000)
	var rec recorder
	require.NoError(t, Enforce(home, PolicyFor(cfg), rec.logger()))
	assert.Equal(t, fs.FileMode(0), modeOf(t, p))
	assert.Contains(t, rec.warn, "could not set file permissions "+p)
}

func TestEnforceDataDirErrors(t *testing.T) {
	noExecutable(t)
	t.Run("cannot be created", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "dangling")
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing", "x"), link))
		err := Enforce(link, Policy{Root: config.Modes{Dir: 0o700}}, Logger{})
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "perms: create "), err.Error())
	})

	t.Run("cannot be resolved", func(t *testing.T) {
		err := Enforce(fileIn(t), Policy{Root: config.Modes{Dir: 0o700}}, Logger{})
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "perms: resolve "), err.Error())
	})
}

func TestCheckErrors(t *testing.T) {
	noExecutable(t)
	t.Run("data directory cannot be stat'ed", func(t *testing.T) {
		_, err := Check(fileIn(t), "", Policy{})
		require.Error(t, err)
	})

	t.Run("config cannot be stat'ed", func(t *testing.T) {
		home := t.TempDir()
		_, err := Check(home, fileIn(t), PolicyFor(operatorConfig(t, home)))
		require.Error(t, err)
	})

	t.Run("data directory cannot be read", func(t *testing.T) {
		skipIfRoot(t)
		home := t.TempDir()
		policy := PolicyFor(operatorConfig(t, home))
		require.NoError(t, os.Chmod(home, 0o300))
		t.Cleanup(func() {
			if err := os.Chmod(home, 0o700); err != nil {
				t.Error(err)
			}
		})
		_, err := Check(home, "", policy)
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "perms: walk "), err.Error())
	})

	t.Run("missing data directory", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "missing")
		got, err := Check(home, "", PolicyFor(operatorConfig(t, home)))
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// An unreadable folder below the data directory is skipped; the rest is
// still set.
func TestEnforceSkipsUnreadableSubtree(t *testing.T) {
	skipIfRoot(t)
	noExecutable(t)
	home := t.TempDir()
	cfg := operatorConfig(t, home)
	locked := place(t, home, "skills/locked/", 0o700)
	inner := place(t, home, "skills/locked/inner.txt", 0o644)
	other := place(t, home, "skills/other.txt", 0o644)
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Error(err)
		}
	})

	got, err := Check(home, "", PolicyFor(cfg))
	require.NoError(t, err)
	paths := make([]string, 0, len(got))
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	assert.Contains(t, paths, other)
	assert.NotContains(t, paths, inner)
}

func TestEnsureFileModeErrors(t *testing.T) {
	err := EnsureFileMode(fileIn(t), 0o600)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "perms: create "), err.Error())

	err = EnsureFileMode(os.DevNull, 0o600)
	require.Error(t, err)
	assert.Equal(t, "perms: "+os.DevNull+": not a regular file", err.Error())
}

func TestCheckConfigStatError(t *testing.T) {
	path := fileIn(t)
	err := CheckConfig(path, 0o600, Logger{})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "perms: stat "+path+": "), err.Error())
}

func TestCheckConfigExactModeUnchanged(t *testing.T) {
	p := place(t, t.TempDir(), "config.json", 0o600)
	var rec recorder
	require.NoError(t, CheckConfig(p, 0o600, rec.logger()))
	assert.Empty(t, rec.info)
	assert.Equal(t, fs.FileMode(0o600), modeOf(t, p))
}

// A data directory the policy leaves unreadable cannot be walked.
func TestEnforceWalkFails(t *testing.T) {
	skipIfRoot(t)
	noExecutable(t)
	home := t.TempDir()
	t.Cleanup(func() {
		if err := os.Chmod(home, 0o700); err != nil {
			t.Error(err)
		}
	})
	err := Enforce(home, Policy{Root: config.Modes{Dir: 0o300}}, Logger{})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "perms: walk "), err.Error())
}

// fakeInfo is a FileInfo with no platform data.
type fakeInfo struct{ fs.FileInfo }

func (fakeInfo) Sys() any { return nil }

func TestLinkCountWithoutPlatformData(t *testing.T) {
	assert.Equal(t, uint64(1), linkCount(fakeInfo{}))
}
