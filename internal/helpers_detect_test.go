package internal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A binary running from inside the install root uses it as its home, and
// CLAW_HOME is set so the rest of the process agrees.
func TestGetClawHome_RunningFromInstallRoot(t *testing.T) {
	t.Setenv("CLAW_HOME", "")
	t.Setenv("HOME", t.TempDir())
	exe, err := os.Executable()
	require.NoError(t, err)
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	saved := installRoot
	installRoot = filepath.Dir(exe)
	t.Cleanup(func() { installRoot = saved })

	assert.Equal(t, installRoot, GetClawHome())
	assert.Equal(t, installRoot, os.Getenv("CLAW_HOME"))
}

// An install root with a config.json owned by this user is used, unless the
// user has their own ~/.claw/config.json.
func TestGetClawHome_InstallRootConfig(t *testing.T) {
	withoutInstallRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(installRoot, "config.json"), []byte("{}"), 0o600))
	home := t.TempDir()

	t.Run("used", func(t *testing.T) {
		t.Setenv("CLAW_HOME", "")
		t.Setenv("HOME", home)
		assert.Equal(t, installRoot, GetClawHome())
	})

	t.Run("user config wins", func(t *testing.T) {
		t.Setenv("CLAW_HOME", "")
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".claw"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".claw", "config.json"), []byte("{}"), 0o600))
		assert.Equal(t, filepath.Join(home, ".claw"), GetClawHome())
		assert.Empty(t, os.Getenv("CLAW_HOME"))
	})
}

// With no home directory known the data directory is a relative .claw.
func TestGetClawHome_NoHomeDirectory(t *testing.T) {
	withoutInstallRoot(t)
	t.Setenv("CLAW_HOME", "")
	t.Setenv("HOME", "")
	assert.Equal(t, ".claw", GetClawHome())
}

func TestLoadConfig_InvalidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	_, err := LoadConfig()
	require.Error(t, err)
}

// A config that cannot be seeded is not fatal: the defaults load.
func TestSeedConfig_UnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	path := filepath.Join(home, "config.json")

	SeedConfig(path)
	assert.NoFileExists(t, path)
}
