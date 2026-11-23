package sessions

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.bison9.com/agent/ctxengine/memory"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/pidfile"
)

// setupHome writes a config with the agents alice and bob and returns the data
// directory.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	cfg := `{"agents":{"list":[{"id":"alice","default":true},{"id":"bob"}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600))
	return home
}

func homeConfig(home string) *config.Config {
	return &config.Config{Agents: config.AgentsConfig{
		BaseDir: filepath.Join(home, "agents"),
		List:    []config.AgentConfig{{ID: "alice", Default: true}, {ID: "bob"}},
	}}
}

func runSessions(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewSessionsCommand()
	var out bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	return out.String(), err
}

// markRunning writes a PID file naming this test process, which counts as a
// running service.
func markRunning(t *testing.T, home string) {
	t.Helper()
	require.NoError(t, pidfile.Write(home))
	_, running := pidfile.Read(home)
	require.True(t, running)
}

func TestNewSessionsCommand(t *testing.T) {
	cmd := NewSessionsCommand()
	assert.Equal(t, "sessions", cmd.Use)
	for _, name := range []string{"erase", "migrate"} {
		sub, _, err := cmd.Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, sub.Name())
	}
}

func TestEraseCommand(t *testing.T) {
	t.Run("requires channel and chat", func(t *testing.T) {
		setupHome(t)
		_, err := runSessions(t, "erase", "--channel", "telegram")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `required flag(s) "chat" not set`)
	})

	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
		_, err := runSessions(t, "erase", "--channel", "telegram", "--chat", "42")
		require.Error(t, err)
	})

	t.Run("refuses while running", func(t *testing.T) {
		home := setupHome(t)
		markRunning(t, home)
		_, err := runSessions(t, "erase", "--channel", "telegram", "--chat", "42")
		require.Error(t, err)
		assert.Equal(t, app.ShortName()+" is running (pid "+itoa(os.Getpid())+"); stop the service before erasing sessions, or use DELETE /api/sessions", err.Error())
	})

	t.Run("erases and prints the report", func(t *testing.T) {
		home := setupHome(t)
		cfg := homeConfig(home)
		dir := seedKeys(t, cfg, "alice", "agent:alice:telegram:direct:42", "agent:alice:telegram:direct:43")

		out, err := runSessions(t, "erase", "--channel", "telegram", "--chat", "42")
		require.NoError(t, err)
		assert.Equal(t, "erased agent:alice:telegram:direct:42\n"+CogmemNote+"\nErased 1 session(s).\n", out)
		assert.False(t, exists(t, dir, "agent:alice:telegram:direct:42"))
		assert.True(t, exists(t, dir, "agent:alice:telegram:direct:43"))
	})

	t.Run("nothing found", func(t *testing.T) {
		setupHome(t)
		out, err := runSessions(t, "erase", "--channel", "telegram", "--chat", "42")
		require.NoError(t, err)
		assert.Equal(t, CogmemNote+"\nNo sessions found for telegram/42.\n", out)
	})
}

func TestEraseReportsDirectoryErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	t.Run("unreadable sessions directory", func(t *testing.T) {
		cfg := eraseTestConfig(t)
		seedKeys(t, cfg, "alice", "agent:alice:telegram:direct:42")
		bobDir := seedKeys(t, cfg, "bob", "agent:bob:telegram:direct:42")
		require.NoError(t, os.Chmod(bobDir, 0o000))
		t.Cleanup(func() {
			if err := os.Chmod(bobDir, 0o700); err != nil {
				t.Error(err)
			}
		})

		rep, err := Erase(cfg, EraseRequest{Channel: "telegram", ChatID: "42"}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "memory: list sessions:")
		assert.Equal(t, []string{"agent:alice:telegram:direct:42"}, rep.Erased, "other agents are still erased")
	})
}

// An archive whose file holds another session is refused, not deleted.
func TestEraseReportsKeyCollision(t *testing.T) {
	cfg := eraseTestConfig(t)
	const mine, other = "agent:alice:telegram:direct:42", "agent:alice:other"
	dir := seedKeys(t, cfg, "alice", mine, other)
	// Swap the two files so each one's name points at the other's key.
	a, b := memory.ArchivePath(dir, mine), memory.ArchivePath(dir, other)
	tmp := a + ".swap"
	require.NoError(t, os.Rename(a, tmp))
	require.NoError(t, os.Rename(b, a))
	require.NoError(t, os.Rename(tmp, b))

	rep, err := Erase(cfg, EraseRequest{Channel: "telegram", ChatID: "42"}, nil)
	require.Error(t, err)
	assert.Empty(t, rep.Erased)
	assert.FileExists(t, a)
	assert.FileExists(t, b)
}

func TestEraseCommandWriteError(t *testing.T) {
	setupHome(t)
	cmd := NewSessionsCommand()
	cmd.SetArgs([]string{"erase", "--channel", "telegram", "--chat", "42"})
	cmd.SetOut(&failAfter{})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorIs(t, cmd.Execute(), errWrite)
}

func TestMigrateCommand(t *testing.T) {
	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
		_, err := runSessions(t, "migrate")
		require.Error(t, err)
	})

	t.Run("refuses while running", func(t *testing.T) {
		home := setupHome(t)
		markRunning(t, home)
		_, err := runSessions(t, "migrate")
		require.Error(t, err)
		assert.Equal(t, app.ShortName()+" is running (pid "+itoa(os.Getpid())+"); stop the service before migrating sessions", err.Error())
	})

	t.Run("migrates every agent", func(t *testing.T) {
		home := setupHome(t)
		dir := filepath.Join(home, "agents", "bob", "sessions")
		writeLegacy(t, dir, "agent:bob:main")
		require.NoError(t, runMigrate())
		_, err := os.Stat(memory.ArchivePath(dir, "agent:bob:main"))
		require.NoError(t, err)
	})
}

// writeLegacy writes a .jsonl + .meta.json pair the way the old store did.
func writeLegacy(t *testing.T, dir, key string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	base := filepath.Join(dir, memory.SanitizeSessionKey(key))
	require.NoError(t, os.WriteFile(base+".jsonl",
		[]byte(`{"seq":1,"created_at":"2026-09-01T10:00:00Z","role":"user","content":"hello"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(base+".meta.json",
		[]byte(`{"key":"`+key+`","count":1,"next_seq":1,"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"}`), 0o600))
}

func TestMigrateDirsReportsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, dir, "agent:alice:main")
	require.NoError(t, migrateDirs(&bytes.Buffer{}, []string{dir}))
	// The same pair again: its DB already holds the window.
	writeLegacy(t, dir, "agent:alice:main")

	var out bytes.Buffer
	require.NoError(t, migrateDirs(&out, []string{dir}))
	assert.Equal(t, "agent:alice:main: skipped (already migrated)\nMigrated 0, skipped 1, errors 0.\n", out.String())
}

func TestMigrateDirsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	bad := t.TempDir()
	require.NoError(t, os.Chmod(bad, 0o000))
	t.Cleanup(func() {
		if err := os.Chmod(bad, 0o700); err != nil {
			t.Error(err)
		}
	})
	good := t.TempDir()
	writeLegacy(t, good, "agent:alice:main")

	var out bytes.Buffer
	err := migrateDirs(&out, []string{bad, good})
	require.Error(t, err)
	assert.Equal(t, "1 session(s) failed to migrate", err.Error())
	assert.Contains(t, out.String(), bad+": error: session: migrate:")
	assert.Contains(t, out.String(), "agent:alice:main: migrated\n")
	assert.True(t, strings.HasSuffix(out.String(), "Migrated 1, skipped 0, errors 1.\n"), out.String())
}

// failAfter accepts n writes and fails every write after them.
type failAfter struct{ n int }

var errWrite = errors.New("write failed")

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errWrite
	}
	f.n--
	return len(p), nil
}

// A failed write of any line stops the migration with that error.
func TestMigrateDirsWriteErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	// Lines, in order: bad dir error, migrated, skipped, orphan error, totals.
	for n := range 5 {
		bad := t.TempDir()
		require.NoError(t, os.Chmod(bad, 0o000))
		t.Cleanup(func() {
			if err := os.Chmod(bad, 0o700); err != nil {
				t.Error(err)
			}
		})
		dir := t.TempDir()
		writeLegacy(t, dir, "agent:alice:main")
		require.NoError(t, migrateDirs(&bytes.Buffer{}, []string{dir}))
		writeLegacy(t, dir, "agent:alice:main")
		writeLegacy(t, dir, "agent:bob:main")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "orphan.jsonl"), []byte(`{"seq":1,"role":"user","content":"x"}`+"\n"), 0o600))

		err := migrateDirs(&failAfter{n: n}, []string{bad, dir})
		require.ErrorIs(t, err, errWrite, "write %d", n)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
