// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// covPaused launches the default configuration, pauses it and restarts
// the service, so the forum is paused on disk with no live controller.
func covPaused(t *testing.T) (*svcEnv, string) {
	t.Helper()
	e := svcSetup(t)
	id, _ := e.launch("")
	e.running(id)
	if err := e.svc.Pause(t.Context(), e.scope, id); err != nil {
		t.Fatal(err)
	}
	e.settled(id, StatusPaused)
	e.restart()
	return e, id
}

// covForumLockFree fails unless another handle can take the forum's lock
// (no operation left it held).
func covForumLockFree(t *testing.T, e *svcEnv, id string) {
	t.Helper()
	h := newForumHandle(e.scope.BaseDirectory, id)
	if err := h.Lock(); err != nil {
		t.Fatalf("the forum's lock is still held: %v", err)
	}
	h.Unlock()
}

func TestSvcNewForumRefusals(t *testing.T) {
	e := svcSetup(t)
	if _, err := e.svc.NewForum(t.Context(), Scope{AgentID: "launcher", BaseDirectory: "relative"}); err == nil {
		t.Error("relative base: want an error")
	}
	file := filepath.Join(e.workspace, "doc.md")
	if _, err := e.svc.NewForum(t.Context(), Scope{AgentID: "launcher", BaseDirectory: file}); err == nil {
		t.Error("base is a file: want an error")
	}
}

func TestSvcNewForumWriteFailureRemovesIt(t *testing.T) {
	e := svcSetup(t)
	// Once the base's housekeeping folders exist, a read-only base refuses
	// the forum's own folder.
	if _, err := e.svc.NewForum(t.Context(), e.scope); err != nil {
		t.Fatal(err)
	}
	covChmod(t, e.scope.BaseDirectory, 0o500)
	if _, err := e.svc.NewForum(t.Context(), e.scope); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("read-only base: %v, want ErrPermission", err)
	}
}

func TestSvcConfigArgumentsMustBeObjects(t *testing.T) {
	e := svcSetup(t)
	id, err := e.svc.NewForum(t.Context(), e.scope)
	if err != nil {
		t.Fatal(err)
	}
	covWantErr(t, "SetConfig", e.svc.SetConfig(t.Context(), e.scope, id, []byte(`[1]`)), "the configuration must be a JSON object")
	covWantErr(t, "UpdateConfig", e.svc.UpdateConfig(t.Context(), e.scope, id, []byte(`"x"`)), "the changes must be a JSON object")
}

func TestSvcEditConfigFailures(t *testing.T) {
	e := svcSetup(t)
	id, err := e.svc.NewForum(t.Context(), e.scope)
	if err != nil {
		t.Fatal(err)
	}
	other := newForumHandle(e.scope.BaseDirectory, id)
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetConfig(t.Context(), e.scope, id, []byte(`{}`)); !errors.Is(err, ErrLocked) {
		t.Errorf("locked: %v, want ErrLocked", err)
	}
	other.Unlock()

	// A forum.json damaged by hand cannot be patched; it can be replaced.
	covWriteFile(t, filepath.Join(e.scope.BaseDirectory, id, fileConfig), "{")
	if err := e.svc.UpdateConfig(t.Context(), e.scope, id, []byte(`{"name":"x"}`)); err == nil {
		t.Error("patch of a damaged configuration: want an error")
	}
	if err := e.svc.SetConfig(t.Context(), e.scope, id, []byte(`{"name":"x"}`)); err != nil {
		t.Errorf("replace a damaged configuration: %v", err)
	}

	// Runs that cannot be listed (the lock's sweep fails).
	runs := filepath.Join(e.scope.BaseDirectory, id, dirRuns)
	covChmod(t, runs, 0)
	if err := e.svc.SetConfig(t.Context(), e.scope, id, []byte(`{}`)); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("runs unreadable: %v, want ErrPermission", err)
	}
	if err := os.Chmod(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	covForumLockFree(t, e, id)
}

func TestSvcUnreadableForumConfig(t *testing.T) {
	e := svcSetup(t)
	id, _ := e.launch("")
	e.running(id)
	covChmod(t, filepath.Join(e.scope.BaseDirectory, id, fileConfig), 0)
	if _, err := e.svc.ExportConfig(t.Context(), e.scope, id); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("export: %v, want ErrPermission", err)
	}
	if err := e.svc.ValidateConfig(t.Context(), e.scope, id, e.opts()); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("validate: %v, want ErrPermission", err)
	}
	if _, err := e.svc.Status(t.Context(), e.scope, id, 0); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("status: %v, want ErrPermission", err)
	}
}

func TestSvcResumeWithUnreadableConfig(t *testing.T) {
	e, id := covPaused(t)
	covChmod(t, filepath.Join(e.scope.BaseDirectory, id, fileConfig), 0)
	if err := e.svc.Resume(t.Context(), e.scope, id); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("resume: %v, want ErrPermission", err)
	}
	covForumLockFree(t, e, id)
}

func TestSvcPauseOfPausedForumOnDisk(t *testing.T) {
	e, id := covPaused(t)
	if err := e.svc.Pause(t.Context(), e.scope, id); err != nil {
		t.Errorf("pause: %v", err)
	}
	covForumLockFree(t, e, id)
}

func TestSvcResumeCommitFailureReleasesLock(t *testing.T) {
	e, id := covPaused(t)
	commits := filepath.Join(e.store(id).Root(), dirCommits)
	covChmod(t, commits, 0o500)
	if err := e.svc.Resume(t.Context(), e.scope, id); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("resume: %v, want ErrPermission", err)
	}
	if err := os.Chmod(commits, 0o700); err != nil {
		t.Fatal(err)
	}
	covForumLockFree(t, e, id)
	if got := e.status(id); got != StatusPaused {
		t.Errorf("status = %s, want paused", got)
	}
}

func TestSvcResumeOfQueuedRun(t *testing.T) {
	// A launch that died after its snapshot but before CommitLaunched
	// leaves a queued run; Resume launches it.
	e := svcSetup(t)
	id, _ := e.launch("")
	e.running(id)
	e.restart()
	s := e.store(id)
	if err := os.Remove(filepath.Join(s.Root(), filepath.FromSlash(commitRel(1)))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Root(), fileState)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if got := e.status(id); got != StatusQueued {
		t.Fatalf("status = %s, want queued", got)
	}
	if err := e.svc.Resume(t.Context(), e.scope, id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	e.running(id)
}

func TestSvcControlAfterClose(t *testing.T) {
	e, id := covPaused(t)
	svcClose(t, e.svc)
	if err := e.svc.Resume(t.Context(), e.scope, id); !errors.Is(err, errClosed) {
		t.Errorf("resume after close: %v, want errClosed", err)
	}
	if err := e.svc.Cancel(t.Context(), e.scope, id); !errors.Is(err, errClosed) {
		t.Errorf("cancel after close: %v, want errClosed", err)
	}
	covForumLockFree(t, e, id)
}

func TestSvcControlWhenControllerCannotOpen(t *testing.T) {
	e, id := covPaused(t)
	e.ctrls.openErr = errors.New("cannot open")
	covWantErr(t, "resume", e.svc.Resume(t.Context(), e.scope, id), "cannot open")
	covForumLockFree(t, e, id)
}

func TestSvcControlOfDamagedRun(t *testing.T) {
	e, id := covPaused(t)
	// The run's copy of the configuration no longer matches its digest.
	covWriteFile(t, filepath.Join(e.store(id).Root(), fileConfig), `{"version":1}`)
	if err := e.svc.Cancel(t.Context(), e.scope, id); !errors.Is(err, ErrCorrupt) {
		t.Errorf("cancel: %v, want ErrCorrupt", err)
	}
	covForumLockFree(t, e, id)
}

func TestSvcDeleteDamagedForum(t *testing.T) {
	t.Run("own forum", func(t *testing.T) {
		e, id := covPaused(t)
		if err := os.RemoveAll(filepath.Join(e.scope.BaseDirectory, id, dirRuns)); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.Delete(t.Context(), e.scope, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if len(e.forumIDs()) != 0 {
			t.Error("the forum is still listed")
		}
	})
	t.Run("another agent's forum", func(t *testing.T) {
		e, id := covPaused(t)
		if err := os.RemoveAll(filepath.Join(e.scope.BaseDirectory, id, dirRuns)); err != nil {
			t.Fatal(err)
		}
		other := Scope{AgentID: "bob", BaseDirectory: e.scope.BaseDirectory}
		if err := e.svc.Delete(t.Context(), other, id); !errors.Is(err, errForeign) {
			t.Errorf("delete: %v, want errForeign", err)
		}
		if _, err := os.Stat(filepath.Join(e.scope.BaseDirectory, id)); err != nil {
			t.Errorf("the forum was removed: %v", err)
		}
	})
}

func TestSvcListUnreadableBase(t *testing.T) {
	e := svcSetup(t)
	scope := Scope{AgentID: "launcher", BaseDirectory: filepath.Join(e.workspace, "doc.md")}
	if _, err := e.svc.List(t.Context(), scope); err == nil {
		t.Error("list of a file: want an error")
	}
}

func TestSvcStatusOfEarlierRunWithDamagedLatest(t *testing.T) {
	e := svcSetup(t)
	id, c := e.launch("")
	c.finish <- StatusCompleted
	e.settled(id, StatusCompleted)
	svcRelaunch(t, e, id)
	e.running(id)
	e.restart() // read run 2 from disk, not from its live controller
	if err := os.Remove(filepath.Join(e.store(id).Root(), fileConfig)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Status(t.Context(), e.scope, id, 1); !errors.Is(err, ErrCorrupt) {
		t.Errorf("status of run 1: %v, want ErrCorrupt", err)
	}
}

func TestSvcStatusRunsUnreadable(t *testing.T) {
	e := svcSetup(t)
	id, err := e.svc.NewForum(t.Context(), e.scope)
	if err != nil {
		t.Fatal(err)
	}
	covChmod(t, filepath.Join(e.scope.BaseDirectory, id, dirRuns), 0)
	if _, err := e.svc.Status(t.Context(), e.scope, id, 0); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("status: %v, want ErrPermission", err)
	}
	if _, err := e.svc.Results(t.Context(), e.scope, id, 0); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("results: %v, want ErrPermission", err)
	}
}

func TestSvcResultsOfDamagedRuns(t *testing.T) {
	t.Run("snapshot does not decode", func(t *testing.T) {
		e, id := covPaused(t)
		covWriteFile(t, filepath.Join(e.store(id).Root(), fileSnapshot), "{")
		if _, err := e.svc.Results(t.Context(), e.scope, id, 0); !errors.Is(err, ErrCorrupt) {
			t.Errorf("results: %v, want ErrCorrupt", err)
		}
	})
	t.Run("paused run has a partial result", func(t *testing.T) {
		e, id := covPaused(t)
		res, err := e.svc.Results(t.Context(), e.scope, id, 0)
		if err != nil || res.Status != StatusPaused || res.Complete {
			t.Errorf("results = %+v, %v", res, err)
		}
	})
	t.Run("configuration copy missing", func(t *testing.T) {
		e, id := covPaused(t)
		if err := os.Remove(filepath.Join(e.store(id).Root(), fileConfig)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Results(t.Context(), e.scope, id, 0); !errors.Is(err, ErrCorrupt) {
			t.Errorf("results: %v, want ErrCorrupt", err)
		}
	})
	t.Run("configuration copy does not decode", func(t *testing.T) {
		e, id := covPaused(t)
		covWriteFile(t, filepath.Join(e.store(id).Root(), fileConfig), `{"version":"one"}`)
		if _, err := e.svc.Results(t.Context(), e.scope, id, 0); !errors.Is(err, ErrCorrupt) {
			t.Errorf("results: %v, want ErrCorrupt", err)
		}
	})
	t.Run("result names another run", func(t *testing.T) {
		e := svcSetup(t)
		id, c := e.launch("")
		c.finish <- StatusCompleted
		e.settled(id, StatusCompleted)
		s := e.store(id)
		res, err := s.ReadResult()
		if err != nil {
			t.Fatal(err)
		}
		res.Run = 9
		data, err := marshalRecord(res)
		if err != nil {
			t.Fatal(err)
		}
		covWriteFile(t, filepath.Join(s.Root(), fileResult), string(data))
		_, err = e.svc.Results(t.Context(), e.scope, id, 0)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "names forum") {
			t.Errorf("results: %v, want ErrCorrupt", err)
		}
	})
	t.Run("result does not decode", func(t *testing.T) {
		e := svcSetup(t)
		id, c := e.launch("")
		c.finish <- StatusCompleted
		e.settled(id, StatusCompleted)
		covWriteFile(t, filepath.Join(e.store(id).Root(), fileResult), "{")
		if _, err := e.svc.Results(t.Context(), e.scope, id, 0); !errors.Is(err, ErrCorrupt) {
			t.Errorf("results: %v, want ErrCorrupt", err)
		}
	})
}
