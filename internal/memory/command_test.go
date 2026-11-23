package memory

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.bison9.com/agent/cogmem/store"

	"go.bison9.com/agency/cogmemhost"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	return string(<-done)
}

// setupHome writes a config with the agents alice and bob and returns the data
// directory.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	cfg := `{"agents":{"list":[{"id":"alice"},{"id":"bob"}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600))
	return home
}

func dbPath(home, agent string) string {
	return store.DBPath(cogmemhost.Dir(filepath.Join(home, "agents", agent)))
}

// seedStore creates an agent's memory with one active memory, one retired
// memory and one archived domain holding one memory.
func seedStore(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	ctx := context.Background()
	db := s.DB()
	keep, err := s.CreateDomain(ctx, db, store.CreateDomainParams{Name: "Keep", Status: store.StatusActive})
	require.NoError(t, err)
	_, err = s.AddMemory(ctx, db, store.AddMemoryParams{DomainID: keep.ID, Type: store.TypeFact, Text: "active", Status: store.StatusActive, Confidence: 0.9})
	require.NoError(t, err)
	_, err = s.AddMemory(ctx, db, store.AddMemoryParams{DomainID: keep.ID, Type: store.TypeFact, Text: "retired", Status: store.StatusRetired, Confidence: 0.9})
	require.NoError(t, err)
	arch, err := s.CreateDomain(ctx, db, store.CreateDomainParams{Name: "Arch", Status: store.StatusActive})
	require.NoError(t, err)
	_, err = s.AddMemory(ctx, db, store.AddMemoryParams{DomainID: arch.ID, Type: store.TypeFact, Text: "archived", Status: store.StatusActive, Confidence: 0.9})
	require.NoError(t, err)
	require.NoError(t, s.ArchiveDomain(ctx, db, arch.ID))
}

// purgeable counts what a purge would still remove from path.
func purgeable(t *testing.T, path string) store.PurgeStats {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	st, err := s.PurgeNonActive(context.Background(), false)
	require.NoError(t, err)
	return st
}

func runMemory(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := NewMemoryCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return out, err
}

func TestNewMemoryCommand(t *testing.T) {
	cmd := NewMemoryCommand()
	assert.Equal(t, "memory", cmd.Use)
	purge, _, err := cmd.Find([]string{"purge"})
	require.NoError(t, err)
	assert.Equal(t, "purge", purge.Use)
	assert.NotNil(t, purge.Flags().Lookup("confirm"))
}

func TestPurgeRejectsArgs(t *testing.T) {
	setupHome(t)
	_, err := runMemory(t, "purge", "extra")
	require.Error(t, err)
}

func TestPurgeBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
	_, err := runMemory(t, "purge")
	require.Error(t, err)
}

func TestPurgeNoDatabases(t *testing.T) {
	setupHome(t)
	out, err := runMemory(t, "purge")
	require.NoError(t, err)
	assert.Equal(t, "No cognitive-memory databases found.\n", out)
}

func TestPurgeDryRun(t *testing.T) {
	home := setupHome(t)
	alice := dbPath(home, "alice")
	seedStore(t, alice)

	out, err := runMemory(t, "purge")
	require.NoError(t, err)
	assert.Equal(t, "DRY RUN across 1 database(s) — re-run with --confirm to delete\n"+
		"  "+alice+": would remove 2 memory(ies), 1 domain(s)\n"+
		"Would remove 2 memory(ies) and 1 domain(s) across 1 database(s).\n", out)
	assert.Equal(t, store.PurgeStats{Memories: 2, Domains: 1}, purgeable(t, alice))
}

func TestPurgeConfirm(t *testing.T) {
	home := setupHome(t)
	alice := dbPath(home, "alice")
	bob := dbPath(home, "bob")
	seedStore(t, alice)
	// Bob's memory has nothing to purge.
	s, err := store.Open(bob)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	out, err := runMemory(t, "purge", "--confirm")
	require.NoError(t, err)
	assert.Equal(t, "PURGING non-active memory across 2 database(s)\n"+
		"  "+alice+": removed 2 memory(ies), 1 domain(s)\n"+
		"Removed 2 memory(ies) and 1 domain(s) across 2 database(s).\n", out)
	assert.Equal(t, store.PurgeStats{}, purgeable(t, alice))
}

func TestPurgeOpenErrorSkipsDatabase(t *testing.T) {
	home := setupHome(t)
	alice := dbPath(home, "alice")
	bob := dbPath(home, "bob")
	seedStore(t, bob)
	require.NoError(t, os.MkdirAll(filepath.Dir(alice), 0o700))
	require.NoError(t, os.WriteFile(alice, bytes.Repeat([]byte("not a database "), 100), 0o600))

	out, err := runMemory(t, "purge")
	require.NoError(t, err)
	assert.Contains(t, out, "  "+alice+": open error: ")
	assert.Contains(t, out, "  "+bob+": would remove 2 memory(ies), 1 domain(s)\n")
	assert.Contains(t, out, "Would remove 2 memory(ies) and 1 domain(s) across 2 database(s).\n")
}

// A purge that fails in one database is reported and left out of the totals.
func TestPurgeErrorIsReported(t *testing.T) {
	home := setupHome(t)
	alice := dbPath(home, "alice")
	seedStore(t, alice)
	s, err := store.Open(alice)
	require.NoError(t, err)
	_, err = s.DB().Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON memories BEGIN SELECT RAISE(ABORT, 'deletes blocked'); END`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	out, err := runMemory(t, "purge", "--confirm")
	require.NoError(t, err)
	assert.Regexp(t, `^PURGING non-active memory across 1 database\(s\)\n`+
		`  `+regexp.QuoteMeta(alice)+`: error: .*deletes blocked.*\n`+
		`Removed 0 memory\(ies\) and 0 domain\(s\) across 1 database\(s\)\.\n$`, out)
	assert.Equal(t, store.PurgeStats{Memories: 2, Domains: 1}, purgeable(t, alice))
}
