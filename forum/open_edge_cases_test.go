// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// edgeExistsFails is svcAgents whose Exists always fails.
type edgeExistsFails struct{ *svcAgents }

func (edgeExistsFails) Exists(context.Context, string) (bool, error) {
	return false, errors.New("registry down")
}

// edgeLaunched is a launched stConfig run, locked as a controller holds it.
func edgeLaunched(t *testing.T) *forumStore {
	t.Helper()
	s := stNewStore(t)
	stLaunch(t, s, stConfig())
	if err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Unlock)
	return s
}

func edgeHost() Host {
	return Host{Agents: svcNewAgents(), Logger: &svcLogger{}}
}

func TestOpenForumFailures(t *testing.T) {
	ctx := t.Context()
	t.Run("sweep fails", func(t *testing.T) {
		s := edgeLaunched(t)
		closed := filepath.Join(s.root, dirLayers, "closed")
		if err := os.Mkdir(closed, 0o700); err != nil {
			t.Fatal(err)
		}
		chmodRestored(t, closed, 0)
		_, err := openForum(ctx, s, edgeHost())
		wantErrContains(t, "openForum", err, "open forum: sweep temporary files")
	})
	t.Run("records damaged", func(t *testing.T) {
		s := edgeLaunched(t)
		writeTestFile(t, filepath.Join(s.root, fileConfig), `{"version":1}`)
		if _, err := openForum(ctx, s, edgeHost()); !errors.Is(err, ErrCorrupt) {
			t.Errorf("openForum: %v, want ErrCorrupt", err)
		}
	})
	t.Run("not locked", func(t *testing.T) {
		s := stNewStore(t)
		stLaunch(t, s, stConfig())
		if _, err := openForum(ctx, s, edgeHost()); !errors.Is(err, ErrInvalidState) {
			t.Errorf("openForum: %v, want ErrInvalidState", err)
		}
	})
	t.Run("request unreadable", func(t *testing.T) {
		s := edgeLaunched(t)
		turn := turnID(1, "alice")
		stReserve(t, s, "debate", 1, turn, TurnParticipant, "alice", 1, 1, "first")
		chmodRestored(t, filepath.Join(s.root, filepath.FromSlash(attemptRel("debate", turn, 1)), fileRequest), 0)
		if _, err := openForum(ctx, s, edgeHost()); !errors.Is(err, ErrCorrupt) {
			t.Errorf("openForum: %v, want ErrCorrupt", err)
		}
	})
	t.Run("moderator schema does not compile", func(t *testing.T) {
		s := edgeLaunched(t)
		edgeRewriteSnapshot(t, s, func(snap *Snapshot) {
			snap.ModeratorSchemas = map[string]json.RawMessage{"debate": json.RawMessage(`{"type":5}`)}
		})
		_, err := openForum(ctx, s, edgeHost())
		wantErrContains(t, "openForum", err, `compile moderator schema of layer "debate"`)
	})
	t.Run("named schema does not compile", func(t *testing.T) {
		s := edgeLaunched(t)
		cfg := stConfig()
		cfg.Schemas = map[string]json.RawMessage{"bad": json.RawMessage(`{"type":5}`)}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(s.root, fileConfig), string(raw))
		edgeRewriteSnapshot(t, s, func(snap *Snapshot) { snap.ConfigDigest = digest(raw) })
		_, err = openForum(ctx, s, edgeHost())
		wantErrContains(t, "openForum", err, `compile schema "bad"`)
	})
	t.Run("agent registry fails", func(t *testing.T) {
		s := edgeLaunched(t)
		host := edgeHost()
		host.Agents = edgeExistsFails{svcNewAgents()}
		_, err := openForum(ctx, s, host)
		wantErrContains(t, "openForum", err, "check participant", "registry down")
	})
	t.Run("transcript is a folder", func(t *testing.T) {
		s := edgeLaunched(t)
		if err := os.Mkdir(filepath.Join(s.root, fileTranscript), 0o700); err != nil {
			t.Fatal(err)
		}
		host := edgeHost()
		host.Agents = edgeAllExist{svcNewAgents()}
		_, err := openForum(ctx, s, host)
		if err == nil || !strings.HasPrefix(err.Error(), "open forum: ") {
			t.Errorf("openForum: %v", err)
		}
	})
}

// edgeAllExist is svcAgents for which every agent exists.
type edgeAllExist struct{ *svcAgents }

func (edgeAllExist) Exists(context.Context, string) (bool, error) { return true, nil }

func TestControllerParticipantsAccessor(t *testing.T) {
	s := edgeLaunched(t)
	host := edgeHost()
	host.Agents = edgeAllExist{svcNewAgents()}
	c, err := openForum(t.Context(), s, host)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Participants(); got == nil || len(got.Participants) != 3 {
		t.Errorf("Participants = %+v", got)
	}
}

func TestSystemClockAfter(t *testing.T) {
	select {
	case <-systemClock{}.After(0):
	case <-time.After(5 * time.Second):
		t.Fatal("systemClock.After(0) never fired")
	}
}

// edgeCleanupPending completes a forum whose temporary agent cannot be
// deleted, so its deletion waits for a retry.
func edgeCleanupPending(t *testing.T) (*svcEnv, string) {
	t.Helper()
	e := svcSetup(t)
	id, c := e.launch("")
	e.agents.setDeleteErr(e.agents.createdIDs()[0], errSvcHost)
	c.finish <- StatusCompleted
	e.settled(id, StatusCompleted)
	if e.cleanupsPending() != 1 {
		t.Fatal("the failed deletion is not queued for a retry")
	}
	return e, id
}

func TestSvcCleanupRetryOfRemovedForum(t *testing.T) {
	e, id := edgeCleanupPending(t)
	if err := os.RemoveAll(filepath.Join(e.scope.BaseDirectory, id)); err != nil {
		t.Fatal(err)
	}
	e.svc.keepAliveTick(t.Context())
	if e.cleanupsPending() != 0 {
		t.Error("the retry of a removed forum is still pending")
	}
}

func TestSvcCleanupRetryOfLockedOrForeignForum(t *testing.T) {
	e, id := edgeCleanupPending(t)
	h := newForumHandle(e.scope.BaseDirectory, id)
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	e.svc.keepAliveTick(t.Context())
	h.Unlock()
	if e.cleanupsPending() != 1 {
		t.Error("the retry of a locked forum was dropped")
	}
	writeTestFile(t, filepath.Join(e.scope.BaseDirectory, id, fileMeta), `{"owner":"bob"}`)
	e.svc.keepAliveTick(t.Context())
	if e.cleanupsPending() != 1 {
		t.Error("the retry of an unreadable forum was dropped")
	}
	if !e.logger.has("temporary agents") {
		t.Error("the failure was not logged")
	}
}
