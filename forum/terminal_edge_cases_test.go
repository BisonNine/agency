// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"os"
	"path/filepath"
	"testing"
)

// edgeNoticeFailed completes a forum whose notice fails once and waits for
// the failed try to be recorded.
func edgeNoticeFailed(t *testing.T) (*svcEnv, string) {
	t.Helper()
	e := svcSetup(t)
	e.notifier.err = errSvcHost
	id, c := e.launch("")
	c.finish <- StatusCompleted
	e.settled(id, StatusCompleted)
	svcEventually(t, "failed try recorded", func() bool { return e.noticeTries(id) == 1 && !e.notifying(id) })
	return e, id
}

func TestSvcNoticeRetryOfRemovedForum(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	if err := os.RemoveAll(filepath.Join(e.scope.BaseDirectory, id)); err != nil {
		t.Fatal(err)
	}
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 0 {
		t.Errorf("tries after the forum is gone = %d, want 0 (forgotten)", got)
	}
	if n := e.notifier.count(); n != 1 {
		t.Errorf("notifier called %d times, want 1", n)
	}
}

func TestSvcNoticeRetryOfLockedForum(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	h := newForumHandle(e.scope.BaseDirectory, id)
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	defer h.Unlock()
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 1 {
		t.Errorf("tries = %d, want 1 (tried again at the next tick)", got)
	}
	if n := e.notifier.count(); n != 1 {
		t.Errorf("notifier called %d times, want 1", n)
	}
}

func TestSvcNoticeRetryOfForeignForum(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	writeTestFile(t, filepath.Join(e.scope.BaseDirectory, id, fileMeta), `{"owner":"bob"}`)
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 1 {
		t.Errorf("tries = %d, want 1", got)
	}
	if !e.logger.has("completion notice") {
		t.Error("the failure was not logged")
	}
}

func TestSvcNoticeRetryOfDamagedRun(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	writeTestFile(t, filepath.Join(e.store(id).Root(), fileConfig), `{"version":1}`)
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 2 {
		t.Errorf("tries = %d, want 2 (the damaged run counts as a failed try)", got)
	}
	if n := e.notifier.count(); n != 1 {
		t.Errorf("notifier called %d times, want 1", n)
	}
}

func TestSvcNoticeRetrySkipsRunningForum(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	svcRelaunch(t, e, id)
	e.running(id)
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 1 {
		t.Errorf("tries = %d, want 1 (not retried while the forum runs)", got)
	}
}

func TestSvcNoticeUnreadableMarker(t *testing.T) {
	e, id := edgeNoticeFailed(t)
	chmodRestored(t, filepath.Join(e.scope.BaseDirectory, dirCleanup, id+".1."+cleanupNotice), 0)
	e.svc.keepAliveTick(t.Context())
	if got := e.noticeTries(id); got != 2 {
		t.Errorf("tries = %d, want 2", got)
	}
	if n := e.notifier.count(); n != 1 {
		t.Errorf("notifier called %d times, want 1 (the marker could not be read)", n)
	}
}

func TestAgentsMarkerCorrupt(t *testing.T) {
	s := stNewStore(t)
	if err := s.SetCleanup(cleanupAgents, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := agentsMarker(s); err == nil {
		t.Error("a marker that does not decode: want an error")
	}
}
