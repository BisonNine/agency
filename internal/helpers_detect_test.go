package internal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

// A binary running from inside the install root uses it as its home, and
// AGENCY_HOME is set so the rest of the process agrees.
func TestGetAgencyHome_RunningFromInstallRoot(t *testing.T) {
	t.Setenv("AGENCY_HOME", "")
	t.Setenv("HOME", t.TempDir())
	exe, err := os.Executable()
	require.NoError(t, err)
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	saved := installRoot
	installRoot = filepath.Dir(exe)
	t.Cleanup(func() { installRoot = saved })

	assert.Equal(t, installRoot, GetAgencyHome())
	assert.Equal(t, installRoot, os.Getenv("AGENCY_HOME"))
}

// An install root with a config.json owned by this user is used, unless the
// user has their own ~/.agency/config.json.
func TestGetAgencyHome_InstallRootConfig(t *testing.T) {
	withoutInstallRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(installRoot, "config.json"), []byte("{}"), 0o600))
	home := t.TempDir()

	t.Run("used", func(t *testing.T) {
		t.Setenv("AGENCY_HOME", "")
		t.Setenv("HOME", home)
		assert.Equal(t, installRoot, GetAgencyHome())
	})

	t.Run("user config wins", func(t *testing.T) {
		t.Setenv("AGENCY_HOME", "")
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".agency"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".agency", "config.json"), []byte("{}"), 0o600))
		assert.Equal(t, filepath.Join(home, ".agency"), GetAgencyHome())
		assert.Empty(t, os.Getenv("AGENCY_HOME"))
	})
}

// With no home directory known the data directory is a relative .agency.
func TestGetAgencyHome_NoHomeDirectory(t *testing.T) {
	withoutInstallRoot(t)
	t.Setenv("AGENCY_HOME", "")
	t.Setenv("HOME", "")
	assert.Equal(t, ".agency", GetAgencyHome())
}

func TestLoadConfig_InvalidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENCY_HOME", home)
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
	t.Cleanup(func() {
		if err := os.Chmod(home, 0o700); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(home, "config.json")

	SeedConfig(path)
	assert.NoFileExists(t, path)
}

// Only the current names count: the old home variable and data directory
// are not looked at, and the system install location is /opt/agency.
func TestGetAgencyHome_OldNamesIgnored(t *testing.T) {
	withoutInstallRoot(t)
	home := t.TempDir()
	old := filepath.Join(home, ".claw") // rename-guard:old-name
	require.NoError(t, os.MkdirAll(old, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(old, "config.json"), []byte("{}"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("CLAW_HOME", old) // rename-guard:old-name

	t.Run("default", func(t *testing.T) {
		t.Setenv("AGENCY_HOME", "")
		assert.Equal(t, filepath.Join(home, ".agency"), GetAgencyHome())
	})
	t.Run("AGENCY_HOME wins", func(t *testing.T) {
		t.Setenv("AGENCY_HOME", "/srv/alice")
		assert.Equal(t, "/srv/alice", GetAgencyHome())
	})
	assert.Equal(t, "/opt/agency", global.SystemDataDir)
}

// TestSeedConfig_NewInstallHasWebUIChat: the seeded config of a new install
// has the WebUI chat channel on, with a token, in a file only the owner can
// read.
func TestSeedConfig_NewInstallHasWebUIChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	SeedConfig(path)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Channels.WebUI.Enabled)
	assert.Len(t, cfg.Channels.WebUI.Token, 32)
	assert.Equal(t, []string{"*"}, []string(cfg.Channels.WebUI.AllowFrom))
	assert.True(t, cfg.DefaultConfig, "still marked as never saved, for the setup wizard")
}
