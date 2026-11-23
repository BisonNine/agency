package audit

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WithRetention sets how far back the prune at Open keeps rows.
func TestWithRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s, err := Open(context.Background(), path, withClock(func() time.Time { return now }))
	require.NoError(t, err)
	s.Record(Event{TS: now.Add(-3 * time.Hour), Kind: "old"})
	s.Record(Event{TS: now.Add(-time.Hour), Kind: "recent"})
	flush(t, s)
	require.NoError(t, s.Close())

	s, err = Open(context.Background(), path, withClock(func() time.Time { return now }), WithRetention(2*time.Hour), WithRetention(0))
	require.NoError(t, err)
	require.NoError(t, s.Close()) // waits for the pruner's first run

	s = openAt(t, path)
	got, err := s.Query(context.Background(), Filter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "recent", got[0].Kind)
}

// openAt opens path with a clock far enough back that its prune keeps every
// row, closing it when the test ends.
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, withClock(func() time.Time { return time.Unix(0, 0) }))
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestOpenErrors(t *testing.T) {
	t.Run("directory cannot be created", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		_, err := Open(context.Background(), filepath.Join(file, "sub", FileName))
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "audit: create directory: "), err.Error())
	})

	t.Run("path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Open(context.Background(), dir)
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "audit: create "+dir+": "), err.Error())
	})

	t.Run("not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("not a database ", 100)), 0o600))
		_, err := Open(context.Background(), path)
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "audit: schema: "), err.Error())
	})
}

// The cut lands on a rune boundary, so the result stays valid UTF-8.
func TestTruncateDetailsRuneBoundary(t *testing.T) {
	// Two-byte runes after one ASCII byte: an even cut lands mid-rune.
	in := "a" + strings.Repeat("é", MaxDetailsBytes)
	out := TruncateDetails(in)
	assert.True(t, utf8.ValidString(out))
	assert.True(t, strings.HasSuffix(out, "…(truncated)"))
	assert.LessOrEqual(t, len(out), MaxDetailsBytes)
}

// A batch that fails to insert is lost, and the writer carries on.
func TestWriteFailureLosesBatchOnly(t *testing.T) {
	s := openTestStore(t)
	_, err := s.db.Exec(`CREATE TRIGGER no_insert BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(t, err)
	s.Record(Event{Kind: "lost"})
	flush(t, s)

	_, err = s.db.Exec(`DROP TRIGGER no_insert`)
	require.NoError(t, err)
	s.Record(Event{Kind: "kept"})
	flush(t, s)

	got, err := s.Query(context.Background(), Filter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "kept", got[0].Kind)

	// Without the table every statement fails: the batch is lost again and
	// queries and prunes report the error.
	_, err = s.db.Exec(`DROP TABLE events`)
	require.NoError(t, err)
	s.Record(Event{Kind: "lost too"})
	flush(t, s)
	_, err = s.Query(context.Background(), Filter{})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "audit: query: "), err.Error())
	_, err = s.Prune(context.Background(), time.Now())
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "audit: prune: "), err.Error())
}

func TestQueryScanError(t *testing.T) {
	s := openTestStore(t)
	_, err := s.db.Exec(`INSERT INTO events (ts, kind) VALUES ('not a time', 'x')`)
	require.NoError(t, err)
	_, err = s.Query(context.Background(), Filter{})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "audit: scan: "), err.Error())
}

func TestPruneNilStore(t *testing.T) {
	var s *Store
	n, err := s.Prune(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Flush gives up when its context ends before the queue is written.
func TestFlushHonoursContext(t *testing.T) {
	s := openTestStore(t)
	s.pending.Add(1) // an event the writer will never see
	t.Cleanup(func() { s.pending.Add(-1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.Flush(ctx), context.Canceled)
}

func TestActorFromNilContext(t *testing.T) {
	//nolint:staticcheck // SA1012: a nil context is the case under test
	assert.Empty(t, ActorFromContext(nil))
}

func TestInitError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	err := Init(file)
	require.Error(t, err)
	assert.Nil(t, Default(), "a failed Init installs nothing")
	assert.False(t, errors.Is(err, ErrClosed))
}

// A prune that fails at Open is logged and leaves the rows in place.
func TestPruneFailureAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	then := now.Add(-400 * 24 * time.Hour)
	// Opened back then, so its own prune keeps the row.
	s, err := Open(context.Background(), path, withClock(func() time.Time { return then }))
	require.NoError(t, err)
	s.Record(Event{TS: then, Kind: "old"})
	flush(t, s)
	_, err = s.db.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = Open(context.Background(), path, withClock(func() time.Time { return now }))
	require.NoError(t, err)
	require.NoError(t, s.Close()) // waits for the pruner's first run

	db, err := sql.Open("sqlite", dsn(path))
	require.NoError(t, err)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n))
	assert.Equal(t, 1, n)
}
