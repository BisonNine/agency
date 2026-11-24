// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package classifier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/classifier/classifierfs"
)

// closedState is a state.db that was opened (schema and migrations applied)
// and closed again, so every statement on it fails.
func closedState(t *testing.T) *stateDB {
	t.Helper()
	st, err := openState(context.Background(), filepath.Join(t.TempDir(), "classifiers"), 0o700, 0o600)
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	if err := st.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return st
}

// Every state operation reports the database's failure instead of acting as
// if it had succeeded (or returning an empty result).
func TestStateOperationsReportDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	st := closedState(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ops := map[string]func() error{
		"processed":     func() error { _, err := st.processed(ctx, "c"); return err },
		"setActed":      func() error { return st.setActed(ctx, "c", "i", now) },
		"dryFailed":     func() error { _, err := st.dryFailed(ctx, "c"); return err },
		"resetAttempts": func() error { return st.resetAttempts(ctx, "c") },
		"gaveUp":        func() error { _, _, err := st.gaveUp(ctx, "c", 3); return err },
		"dryDecided":    func() error { _, err := st.dryDecided(ctx, "c"); return err },
		"setDry":        func() error { return st.setDry(ctx, "c", "i", "Spam", "r", "m", now) },
		"markSeen":      func() error { return st.markSeen(ctx, "processed", "c", []string{"i"}, now) },
		"addActivity":   func() error { return st.addActivity(ctx, "c", ActivityEntry{ItemID: "i", At: now}) },
		"activity":      func() error { _, err := st.activity(ctx, "c", ActivityQuery{}); return err },
		"citations":     func() error { _, err := st.citations(ctx, "c", now); return err },
		"citedLabels":   func() error { _, err := st.citedLabels(ctx, "c"); return err },
		"prune":         func() error { return st.prune(ctx, "c", now, now) },
		"forget":        func() error { return st.forget(ctx, "c") },
		"addRunFailure": func() error {
			return st.addRunFailure(ctx, "c", "boom", "", false, now)
		},
		"markInvalidReported":  func() error { _, err := st.markInvalidReported(ctx, "c", "h"); return err },
		"lastRunFrom":          func() error { _, err := lastRunFrom(ctx, st.db, "c"); return err },
		"counts":               func() error { _, err := st.counts(ctx, "c"); return err },
		"dryCounts":            func() error { _, err := st.dryCounts(ctx, "c"); return err },
		"setRun":               func() error { return st.setRun(ctx, "c", RunRecord{StartedAt: now, EndedAt: now}) },
		"runFailuresFrom":      func() error { _, err := runFailuresFrom(ctx, st.db, "c"); return err },
		"setRunFailures":       func() error { return st.setRunFailures(ctx, "c", failureState{failures: 1}) },
		"runSucceeded":         func() error { return st.runSucceeded(ctx, "c") },
		"clearInvalidReported": func() error { return st.clearInvalidReported(ctx, "c") },
		"clearRunFailures":     func() error { return st.clearRunFailures(ctx, "c") },
		"addAttempts":          func() error { return st.addAttempts(ctx, "processed", "c", []string{"i"}) },
		"setClassified":        func() error { return st.setClassified(ctx, "c", "i", Decision{Category: "Spam"}, now) },
		"setFailed":            func() error { return st.setFailed(ctx, "c", "i", "e", now) },
		"setDryFailed":         func() error { return st.setDryFailed(ctx, "c", "i", "e", now) },
		"setUnclassified":      func() error { return st.setUnclassified(ctx, "c", "i", "m", now) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Errorf("%s on a closed database = nil, want an error", name)
			}
		})
	}
}

// markSeen and addAttempts name only the two item tables; any other is
// refused before a statement is built from it.
func TestStateUnknownTableRefused(t *testing.T) {
	ctx := context.Background()
	st, err := openState(ctx, filepath.Join(t.TempDir(), "classifiers"), 0o700, 0o600)
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	t.Cleanup(func() { closeOrFail(t, st) })
	if err := st.markSeen(ctx, "runs", "c", []string{"i"}, time.Now()); err == nil || !strings.Contains(err.Error(), "unknown table runs") {
		t.Errorf("markSeen(runs) = %v, want unknown table", err)
	}
	if err := st.addAttempts(ctx, "runs", "c", []string{"i"}); err == nil || !strings.Contains(err.Error(), "unknown table runs") {
		t.Errorf("addAttempts(runs) = %v, want unknown table", err)
	}
}

// setActed on an item that has no processed row is an error: the action's
// success would otherwise be lost silently.
func TestSetActedWithoutRowFails(t *testing.T) {
	ctx := context.Background()
	st, err := openState(ctx, filepath.Join(t.TempDir(), "classifiers"), 0o700, 0o600)
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	t.Cleanup(func() { closeOrFail(t, st) })
	if err := st.setActed(ctx, "c", "missing", time.Now()); err == nil || !strings.Contains(err.Error(), "0 rows changed") {
		t.Errorf("setActed(missing) = %v, want 0 rows changed", err)
	}
}

// openState refuses a base that is a file, and a state.db that is not a
// database.
func TestOpenStateFailures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openState(ctx, file, 0o700, 0o600); err == nil {
		t.Error("openState(base is a file) = nil, want an error")
	}
	base := filepath.Join(dir, "classifiers")
	if err := os.MkdirAll(filepath.Join(base, classifierfs.StateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openState(ctx, base, 0o700, 0o600); err == nil {
		t.Error("openState(state.db is a folder) = nil, want an error")
	}
	junk := filepath.Join(dir, "junk")
	if err := os.MkdirAll(junk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, classifierfs.StateFile), []byte(strings.Repeat("not a database ", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openState(ctx, junk, 0o700, 0o600); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("openState(not a database) = %v, want a schema error", err)
	}
}

// CheckState accepts an existing, readable state.db and refuses one that is
// not a database.
func TestCheckStateExistingDatabase(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(t.TempDir(), "classifiers")
	st, err := openState(ctx, base, 0o700, 0o600)
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckState(ctx, base); err != nil {
		t.Errorf("CheckState(valid state.db) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, classifierfs.StateFile), []byte(strings.Repeat("garbage!", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckState(ctx, base); err == nil {
		t.Error("CheckState(not a database) = nil, want an error")
	}
	if err := os.Chmod(filepath.Join(base, classifierfs.StateFile), 0o400); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := CheckState(ctx, base); err == nil {
			t.Error("CheckState(read-only state.db) = nil, want an error")
		}
	}
}

// checkCreatable refuses a base under a file: the nearest existing path is
// not a folder.
func TestCheckStateUnderFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckState(context.Background(), filepath.Join(file, "classifiers"))
	if err == nil {
		t.Fatal("CheckState(under a file) = nil, want an error")
	}
}

// lastRun logs a state read failure and reports no run rather than failing
// the listing it is part of.
func TestLastRunReadFailureIsNoRun(t *testing.T) {
	e := newEnv(t)
	st := closedState(t)
	if got := e.svc.lastRun(context.Background(), st.db, "spam"); got != nil {
		t.Errorf("lastRun on a closed database = %+v, want nil", got)
	}
	if n := e.log.warnings("read last run"); n != 1 {
		t.Errorf("read last run warnings = %d, want 1", n)
	}
}

// writeAtomic fails in a folder that does not exist, and when the rename
// over path fails it leaves no temporary file behind.
func TestWriteAtomicFailures(t *testing.T) {
	dir := t.TempDir()
	if err := writeAtomic(filepath.Join(dir, "missing", "a.json"), []byte("{}"), 0o600, false); err == nil {
		t.Error("writeAtomic(missing folder) = nil, want an error")
	}
	target := filepath.Join(dir, "a.json")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, []byte("{}"), 0o600, false); err == nil {
		t.Error("writeAtomic(over a non-empty folder) = nil, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), classifierfs.TempPrefix) {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

// mkdirAllExact reports a folder it cannot create.
func TestMkdirAllExactUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreMode(t, parent) })
	if err := mkdirAllExact(filepath.Join(parent, "classifiers"), 0o700); err == nil {
		t.Error("mkdirAllExact(under a read-only folder) = nil, want an error")
	}
}

// An invalid definition whose state.db cannot be opened is logged, and the
// agent gets no notice (it would be repeated on every read, uncounted).
// Forgetting the report of a valid file then does nothing.
func TestInvalidReportWithoutState(t *testing.T) {
	e := newEnv(t)
	e.log.expectErrors()
	base := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := Scope{AgentID: "alice", BaseDirectory: base}
	ctx := context.Background()
	e.svc.reportInvalid(ctx, scope, filepath.Join(base, "spam.json"),
		fileInfo{raw: []byte(`{"id":"spam"}`), invalid: "The definition is not valid."})
	if got := e.notifier.allNotices(); len(got) != 0 {
		t.Errorf("notices = %v, want none", got)
	}
	errs := e.log.errors()
	if len(errs) != 1 || !strings.Contains(errs[0], "classifier=spam agent=alice: open state") {
		t.Errorf("error logs = %v, want one open state error", errs)
	}
	e.svc.validAgain(ctx, scope, "spam")
	if got := e.log.errors(); len(got) != 1 {
		t.Errorf("validAgain logged %v, want nothing more", got[1:])
	}
}
