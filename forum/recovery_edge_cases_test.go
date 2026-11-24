// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSvcRecoverUnreadableBase(t *testing.T) {
	e := svcSetup(t)
	scope := Scope{AgentID: "launcher", BaseDirectory: filepath.Join(e.workspace, "doc.md")}
	if err := e.svc.Recover(t.Context(), []Scope{scope}); err == nil {
		t.Error("Recover of a file: want an error")
	}
	if !e.logger.has("forum recovery in " + scope.BaseDirectory) {
		t.Error("the failure was not logged")
	}
}

func TestSvcRecoverStagedRemovalFailure(t *testing.T) {
	e := svcSetup(t)
	id := uuid.NewString()
	stuck := filepath.Join(e.scope.BaseDirectory, dirCleanup, id, "inner")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(stuck, "f"), "x")
	chmodRestored(t, stuck, 0o500)
	err := e.svc.Recover(t.Context(), []Scope{e.scope})
	wantErrContains(t, "Recover", err, "forum "+id)
	if !e.logger.has("finishing its removal") {
		t.Error("the failure was not logged")
	}
}

func TestSvcRecoverIncompleteForums(t *testing.T) {
	e := svcSetup(t)
	if _, err := e.svc.NewForum(t.Context(), e.scope); err != nil { // creates the base
		t.Fatal(err)
	}
	locked := uuid.NewString()
	if err := os.Mkdir(filepath.Join(e.scope.BaseDirectory, locked), 0o700); err != nil {
		t.Fatal(err)
	}
	h := newForumHandle(e.scope.BaseDirectory, locked)
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	defer h.Unlock()
	stuck := uuid.NewString()
	inner := filepath.Join(e.scope.BaseDirectory, stuck, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	// A stale staged copy of the same ID that cannot be removed blocks
	// the removal.
	stale := filepath.Join(e.scope.BaseDirectory, dirCleanup, stuck, "x")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(stale, "f"), "x")
	chmodRestored(t, stale, 0o500)

	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err == nil {
		t.Error("Recover: want the stuck staged copy's error")
	}
	if _, err := os.Stat(filepath.Join(e.scope.BaseDirectory, locked)); err != nil {
		t.Errorf("a forum being created was removed: %v", err)
	}
	if !e.logger.has("forum " + stuck + ": removing an incomplete forum") {
		t.Error("the failed removal was not logged")
	}
}

func TestSvcRecoverNewForumStartsNothing(t *testing.T) {
	e := svcSetup(t)
	if _, err := e.svc.NewForum(t.Context(), e.scope); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err != nil {
		t.Errorf("Recover: %v", err)
	}
	if n := e.ctrls.openCount(); n != 0 {
		t.Errorf("%d controllers opened, want none", n)
	}
}

func TestSvcRecoverDamagedLatestRun(t *testing.T) {
	e, id := edgePaused(t)
	writeTestFile(t, filepath.Join(e.store(id).Root(), fileConfig), `{"version":1}`)
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Recover: %v, want ErrCorrupt", err)
	}
	if len(e.stuck.list()) != 1 {
		t.Errorf("stuck reports = %v, want one", e.stuck.list())
	}
	edgeForumLockFree(t, e, id)
}

func TestSvcRecoverUndoFailure(t *testing.T) {
	e, id := edgePaused(t)
	svcUnstartedRun(t, e, id, 2)
	chmodRestored(t, filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o500)
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err == nil {
		t.Error("Recover: want an error")
	}
	if err := os.Chmod(filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o700); err != nil {
		t.Fatal(err)
	}
	edgeForumLockFree(t, e, id)
}

// edgeTwoRuns leaves a forum whose run 1 completed and run 2 was running when
// the service stopped.
func edgeTwoRuns(t *testing.T) (*svcEnv, string, *forumStore) {
	t.Helper()
	e := svcSetup(t)
	id, c := e.launch("")
	c.finish <- StatusCompleted
	e.settled(id, StatusCompleted)
	svcEventually(t, "notice", func() bool { return e.notifier.count() == 1 })
	svcRelaunch(t, e, id)
	e.running(id)
	e.restart()
	return e, id, newForumHandle(e.scope.BaseDirectory, id).Run(1)
}

func TestSvcRecoverDamagedEarlierRunWithAgents(t *testing.T) {
	e, id, run1 := edgeTwoRuns(t)
	if err := setAgentsMarker(run1, []string{"leftover"}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(run1.Root(), fileConfig), `{"version":1}`)
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	e.running(id)
	if _, ok := e.markerRun(id, 1, cleanupAgents); ok {
		t.Error("the damaged run's agents marker is still there")
	}
	found := false
	for _, a := range e.agents.deletedIDs() {
		found = found || a == "leftover"
	}
	if !found {
		t.Errorf("deleted %v, want the leftover agent", e.agents.deletedIDs())
	}
}

func TestSvcRecoverDamagedEarlierRunWithoutMarkers(t *testing.T) {
	e, id, run1 := edgeTwoRuns(t)
	writeTestFile(t, filepath.Join(run1.Root(), fileSnapshot), "{")
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	e.running(id)
}

func TestSvcRecoverEarlierRunUnreadableMarker(t *testing.T) {
	e, id, run1 := edgeTwoRuns(t)
	if err := run1.SetCleanup(cleanupNotice, []byte("pending\n")); err != nil {
		t.Fatal(err)
	}
	chmodRestored(t, run1.cleanupPath(cleanupNotice), 0)
	if err := e.svc.Recover(t.Context(), []Scope{e.scope}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	e.running(id)
	if !e.logger.has("read cleanup marker") {
		t.Error("the unreadable marker was not logged")
	}
}
