// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMaterialiseSourcesRefusals(t *testing.T) {
	s := stNewStore(t)
	resolved := &resolvedConfig{SourceContents: map[string][]byte{}}
	cases := map[string]struct {
		src  Source
		want string
	}{
		"file not read": {Source{File: "doc.md", Decode: FormatText}, `file "doc.md" was not read by preflight`},
		"bad json":      {Source{Decode: FormatJSON, Inline: json.RawMessage(`{`)}, "inline content"},
		"not a string":  {Source{Decode: FormatText, Inline: json.RawMessage(`1`)}, "inline content is not a JSON string"},
	}
	for name, c := range cases {
		cfg := &Config{Sources: map[string]Source{"s": c.src}}
		_, err := materialiseSources(s, cfg, resolved)
		covWantErr(t, name, err, `source "s"`, c.want)
	}
	if _, err := s.WriteSource("s", FormatText, []byte("x")); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Sources: map[string]Source{"s": {Decode: FormatText, Inline: json.RawMessage(`"y"`)}}}
	_, err := materialiseSources(s, cfg, resolved)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("source written twice: %v, want ErrExist", err)
	}
}

func TestSvcLaunchOfLockedForum(t *testing.T) {
	e := svcSetup(t)
	id, err := e.svc.NewForum(t.Context(), e.scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetConfig(t.Context(), e.scope, id, []byte(svcConfigJSON)); err != nil {
		t.Fatal(err)
	}
	other := newForumHandle(e.scope.BaseDirectory, id)
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	defer other.Unlock()
	if _, err := e.svc.Launch(t.Context(), e.scope, id, e.opts()); !errors.Is(err, ErrLocked) {
		t.Errorf("launch: %v, want ErrLocked", err)
	}
}

func TestSvcLaunchFileFailures(t *testing.T) {
	newForum := func(t *testing.T) (*svcEnv, string) {
		t.Helper()
		e := svcSetup(t)
		id, err := e.svc.NewForum(t.Context(), e.scope)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SetConfig(t.Context(), e.scope, id, []byte(svcConfigJSON)); err != nil {
			t.Fatal(err)
		}
		return e, id
	}
	t.Run("configuration unreadable", func(t *testing.T) {
		e, id := newForum(t)
		covChmod(t, filepath.Join(e.scope.BaseDirectory, id, fileConfig), 0)
		if _, err := e.svc.Launch(t.Context(), e.scope, id, e.opts()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("launch: %v, want ErrPermission", err)
		}
		covForumLockFree(t, e, id)
	})
	t.Run("run cannot be created", func(t *testing.T) {
		e, id := newForum(t)
		covChmod(t, filepath.Join(e.scope.BaseDirectory, id, dirRuns), 0o500)
		if _, err := e.svc.Launch(t.Context(), e.scope, id, e.opts()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("launch: %v, want ErrPermission", err)
		}
		covForumLockFree(t, e, id)
	})
	t.Run("cleanup markers cannot be written", func(t *testing.T) {
		e, id := newForum(t)
		covChmod(t, filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o500)
		if _, err := e.svc.Launch(t.Context(), e.scope, id, e.opts()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("launch: %v, want ErrPermission", err)
		}
		if err := os.Chmod(filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o700); err != nil {
			t.Fatal(err)
		}
		// The launch was undone: no run, and the agent it created before
		// its marker failed is deleted, not left behind.
		runs, err := newForumHandle(e.scope.BaseDirectory, id).Runs()
		if err != nil || len(runs) != 0 {
			t.Errorf("runs after a failed launch = %v, %v", runs, err)
		}
		created, deleted := e.agents.createdIDs(), e.agents.deletedIDs()
		slices.Sort(created)
		slices.Sort(deleted)
		if len(created) == 0 || !slices.Equal(created, deleted) {
			t.Errorf("created %v, deleted %v; want every created agent deleted", created, deleted)
		}
		covForumLockFree(t, e, id)
	})
	t.Run("cleanup markers cannot be written and the agent not deleted", func(t *testing.T) {
		e, id := newForum(t)
		svc := New(Host{
			Messenger: svcMessenger{}, Agents: covFailDelete{e.agents}, Notifier: e.notifier,
			Logger: e.logger, OnStuck: e.stuck.record, Cards: e.cards,
		})
		svc.openCtrl = e.ctrls.open
		t.Cleanup(func() { svcClose(t, svc) })
		covChmod(t, filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o500)
		_, err := svc.Launch(t.Context(), e.scope, id, e.opts())
		if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "delete refused") {
			t.Errorf("launch: %v, want the marker and the deletion failure", err)
		}
	})
	t.Run("leftover run cannot be undone", func(t *testing.T) {
		e, id := newForum(t)
		svcUnstartedRun(t, e, id, 1)
		covChmod(t, filepath.Join(e.scope.BaseDirectory, dirCleanup), 0o500)
		if _, err := e.svc.Launch(t.Context(), e.scope, id, e.opts()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("launch: %v, want ErrPermission", err)
		}
	})
}

func TestSvcLaunchSupersedesDamagedRun(t *testing.T) {
	e := svcSetup(t)
	id, _ := e.launch("")
	e.running(id)
	if err := e.svc.Pause(t.Context(), e.scope, id); err != nil {
		t.Fatal(err)
	}
	e.settled(id, StatusPaused)
	e.restart()
	run1 := e.store(id)
	covWriteFile(t, filepath.Join(run1.Root(), fileConfig), `{"version":1}`)
	n, err := e.svc.Launch(t.Context(), e.scope, id, e.opts())
	if err != nil || n != 2 {
		t.Fatalf("launch over a damaged run = %d, %v; want run 2", n, err)
	}
	e.running(id)
	if got := len(e.agents.deletedIDs()); got != 2 {
		t.Errorf("deleted %d agents of the damaged run, want 2", got)
	}
	if _, ok := e.markerRun(id, 1, cleanupNotice); ok {
		t.Error("the damaged run's notice marker is still there")
	}
	if e.keptAlive(id) {
		t.Error("the damaged run is still kept alive")
	}
	if e.logger.has("superseded by a new run") {
		t.Error("a damaged run was cancelled like a paused one")
	}
}

// covFailDelete is svcAgents whose Delete always fails.
type covFailDelete struct{ *svcAgents }

func (covFailDelete) Delete(context.Context, string, string) error {
	return errors.New("delete refused")
}
