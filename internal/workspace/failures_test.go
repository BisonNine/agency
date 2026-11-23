package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/config"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
}

// A workspace that cannot be created is left alone.
func TestPopulate_WorkspaceNotCreatable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, "x")
	ws := filepath.Join(file, "ws")
	Populate(ws, 0o700, 0o600, []string{config.ChannelKindTelegram}, kindOf)
	_, err := os.Stat(ws)
	assert.Error(t, err)
}

// A files entry that is not a directory is kept; the templates are still seeded.
func TestPopulate_FilesNotADirectory(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "files"), "mine")
	Populate(ws, 0o700, 0o600, nil, kindOf)
	assert.Equal(t, "mine", readFile(t, filepath.Join(ws, "files")))
	assert.FileExists(t, filepath.Join(ws, "AGENTS.md"))
}

// A template folder blocked by a file stops the template walk, but the files
// before it and the channel files are still seeded.
func TestPopulate_TemplateWalkFails(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "skills"), "mine")
	Populate(ws, 0o700, 0o600, []string{config.ChannelKindTelegram}, kindOf)
	assert.Equal(t, "mine", readFile(t, filepath.Join(ws, "skills")))
	assert.FileExists(t, filepath.Join(ws, "AGENTS.md"))
	assert.Equal(t, channelTemplate(t, "TELEGRAM.md"), readFile(t, filepath.Join(ws, "TELEGRAM.md")))
}

func TestSeedChannelFiles_UnknownKindIgnored(t *testing.T) {
	ws := t.TempDir()
	seedChannelFiles(ws, 0o600, []string{"nope", ""})
	entries, err := os.ReadDir(ws)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestSeedChannelFiles_WriteFails(t *testing.T) {
	skipIfRoot(t)
	ws := t.TempDir()
	require.NoError(t, os.Chmod(ws, 0o500))
	t.Cleanup(func() {
		if err := os.Chmod(ws, 0o700); err != nil {
			t.Error(err)
		}
	})
	seedChannelFiles(ws, 0o600, []string{config.ChannelKindTelegram, config.ChannelKindSlack})
	for _, f := range []string{"TELEGRAM.md", "SLACK.md"} {
		assert.NoFileExists(t, filepath.Join(ws, f))
	}
}

func TestMigrateChannelFiles_MissingWorkspace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "missing")
	migrateChannelFiles(ws, kindOf)
	_, err := os.Stat(ws)
	assert.True(t, os.IsNotExist(err))
}

// An old file that cannot be renamed stays where it is.
func TestMigrateChannelFiles_RenameFails(t *testing.T) {
	skipIfRoot(t)
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "channel-telegram.md"), "old")
	require.NoError(t, os.Chmod(ws, 0o500))
	t.Cleanup(func() {
		if err := os.Chmod(ws, 0o700); err != nil {
			t.Error(err)
		}
	})
	migrateChannelFiles(ws, kindOf)
	assert.Equal(t, "old", readFile(t, filepath.Join(ws, "channel-telegram.md")))
	assert.NoFileExists(t, filepath.Join(ws, "TELEGRAM.md"))
}
