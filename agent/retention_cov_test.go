// ClawEh
// License: MIT

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/clock"

	"go.bison9.com/agency/cogmemhost"
	"go.bison9.com/agency/logger"
)

// retCovTicker is a ticker the test drives by hand: each tick is handed
// straight to the reader, so a send returns only once the retention
// goroutine took it (and the next send only once it handled the last).
type retCovTicker struct {
	ch      chan time.Time
	stopped chan struct{}
}

func (k *retCovTicker) C() <-chan time.Time { return k.ch }
func (k *retCovTicker) Stop()               { close(k.stopped) }
func (k *retCovTicker) Reset(time.Duration) {}

// retCovClock is a fake clock whose one ticker is a retCovTicker.
type retCovClock struct {
	*clock.Fake
	ticker *retCovTicker
}

func (c *retCovClock) NewTicker(time.Duration) clock.Ticker { return c.ticker }

// tick hands the retention goroutine one tick at now.
func (k *retCovTicker) tick(t *testing.T, now time.Time) {
	t.Helper()
	select {
	case k.ch <- now:
	case <-time.After(30 * time.Second):
		t.Fatalf("the retention goroutine did not take the tick at %v", now)
	}
}

// retCovBreakSnapshots makes agent ag's cogmem snapshot directory unreadable
// as a directory, so every retention pass fails on it.
func retCovBreakSnapshots(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.WriteFile(cogmemhost.Dir(stateDir), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// StartRetention runs one pass a day at 03:45 (in the tick's zone): a tick
// at another minute does nothing, the 03:45 tick runs the pass (a failed pass alerts),
// a second tick in the same 03:45 does not run it again, and 03:45 the next
// day does. Closing the loop stops it.
func TestRetCov_StartRetentionOncePerDay(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	rec := newLoopCovAlerts()
	tl.al.SetAlerter(rec)
	retCovBreakSnapshots(t, tl.al.GetRegistry().Default().StateDir)

	day1 := time.Date(2026, time.November, 14, retentionHour, retentionMinute, 0, 0, time.UTC)
	ticker := &retCovTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	tl.al.clock = &retCovClock{Fake: clock.NewFake(day1.Add(-time.Minute)), ticker: ticker}

	tl.al.StartRetention()
	ticker.tick(t, day1.Add(-time.Minute)) // 03:44: not the time
	ticker.tick(t, day1)                   // 03:45: runs
	got := rec.next(t)
	if got.EventID != "session-retention" || got.Title != "Session retention failed" {
		t.Fatalf("alert = %+v, want the retention failure", got)
	}
	ticker.tick(t, day1.Add(30*time.Second)) // 03:45 again: already ran today
	ticker.tick(t, day1.AddDate(0, 0, 1))    // 03:45 next day: runs
	if got := rec.next(t); got.EventID != "session-retention" {
		t.Fatalf("second day alert = %+v", got)
	}
	select {
	case extra := <-rec.ch:
		t.Fatalf("the pass ran more than once a day: %+v", extra)
	default:
	}

	close(tl.al.evictStop)
	select {
	case <-ticker.stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the retention goroutine did not stop with the loop")
	}
}

// A pass with nothing to delete reports nothing and raises no alert.
func TestRetCov_RunRetentionPassNothingToDo(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	tl.cfg.Session.RetentionDays = 30
	rec := newLoopCovAlerts()
	tl.al.SetAlerter(rec)

	rep := tl.al.runRetentionPass(time.Now())

	if len(rep.deleted) != 0 || rep.snapshots != 0 || rep.skippedOpen != 0 || len(rep.errors) != 0 {
		t.Fatalf("report = %+v, want nothing done", rep)
	}
	select {
	case a := <-rec.ch:
		t.Fatalf("alert raised for an empty pass: %+v", a)
	default:
	}
}

// A sessions path that is not a directory is reported as an error.
func TestRetCov_PruneSessionsUnreadableDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := pruneSessions(path, nil, time.Now(), 30, nil)
	if len(rep.errors) != 1 || len(rep.deleted) != 0 {
		t.Fatalf("report = %+v, want one error", rep)
	}
}

// An archive that cannot be opened is skipped, not judged.
func TestRetCov_PruneSessionsUnopenableArchive(t *testing.T) {
	dir := t.TempDir()
	const key = "agent:main:telegram:direct:1"
	path := seedRetentionArchive(t, dir, key, time.Now().Add(-400*24*time.Hour), false)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Errorf("restore %s: %v", path, err)
		}
	})

	// An archive name pointing at nothing cannot be opened either.
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling.archive.db")); err != nil {
		t.Fatal(err)
	}

	rep := pruneSessions(dir, nil, time.Now(), 30, nil)

	if len(rep.deleted) != 0 || len(rep.errors) != 0 {
		t.Fatalf("report = %+v, want the archives skipped", rep)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if !archiveExists(t, dir, key) {
		t.Fatal("an archive that could not be opened was deleted")
	}
}

// An idle archive whose files cannot all be removed is reported as an
// error naming the session, and not counted as deleted.
func TestRetCov_PruneSessionsDeleteFails(t *testing.T) {
	dir := t.TempDir()
	const key = "agent:main:telegram:direct:1"
	path := seedRetentionArchive(t, dir, key, time.Now().Add(-400*24*time.Hour), false)
	// A non-empty directory where the shared-memory file goes cannot be
	// removed.
	if err := os.MkdirAll(filepath.Join(path+"-shm", "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	rep := pruneSessions(dir, nil, time.Now(), 30, nil)

	if len(rep.errors) != 1 || len(rep.deleted) != 0 || !strings.Contains(rep.errors[0].Error(), key) {
		t.Fatalf("report = %+v, want one error naming %s", rep, key)
	}
}
