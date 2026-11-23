// ClawEh
// License: MIT

package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/toolkit"
)

// Every spelling of an agent id is one task owner: a task Alice's manager
// started under "Alice" is listed for "alice" and cancelled by a manager of
// "ALICE"; Bob sees none of it.
func TestTaskOwner_CaseInsensitive(t *testing.T) {
	w := newBlockingWorker()
	reg := loop.NewTasks(nil, hook.DefaultCancelGrace)
	ws := t.TempDir()
	started := NewSubagentManager(SubagentManagerConfig{
		Workspace: ws, Live: NewLiveSet(), Tasks: reg, CallerAgentID: "Alice", RunFull: w.run,
	})
	id, err := started.SpawnCallback("work", "job", "", "cli", "direct", "", "", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	<-w.started
	if got := RunningTasks(reg, "alice"); len(got) != 1 || got[0].ID != id || got[0].Owner != "alice" {
		t.Fatalf("alice's tasks = %+v, want the one task, owned by alice", got)
	}
	if got := RunningTasks(reg, "bob"); len(got) != 0 {
		t.Fatalf("bob's tasks = %+v, want none", got)
	}
	other := NewSubagentManager(SubagentManagerConfig{
		Workspace: ws, Live: NewLiveSet(), Tasks: reg, CallerAgentID: "ALICE", RunFull: w.run,
	})
	if res := other.CancelTask(id); res.Cancelled != 1 {
		t.Fatalf("cancel by ALICE = %+v, want the task cancelled", res)
	}
	<-w.released
	if got := RunningTasks(reg, "Alice"); len(got) != 0 {
		t.Fatalf("tasks left = %+v", got)
	}
}

// An empty owner stays empty: it is never taken for another agent.
func TestTaskOwner_Empty(t *testing.T) {
	if got := TaskOwner(" "); got != " " {
		t.Fatalf("TaskOwner(blank) = %q", got)
	}
	if got := TaskOwner("Alice"); got != "alice" {
		t.Fatalf("TaskOwner(Alice) = %q", got)
	}
}

// An agent's running tasks are listed oldest first, whatever order they
// were registered in.
func TestRunningTasks_OldestFirst(t *testing.T) {
	reg := loop.NewTasks(nil, 0) // no workers to wait for
	alice := TasksOf(reg, "alice")
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, s := range []struct {
		id  string
		age time.Duration
	}{{"newest", 0}, {"oldest", 2 * time.Hour}, {"middle", time.Hour}} {
		if _, _, err := alice.StartTask(context.Background(), hook.TaskStart{Info: hook.TaskInfo{ID: s.id, Started: t0.Add(-s.age)}}); err != nil {
			t.Fatal(err)
		}
	}
	running := RunningTasks(reg, "Alice")
	ids := make([]string, 0, len(running))
	for _, info := range running {
		ids = append(ids, info.ID)
	}
	if len(ids) != 3 || ids[0] != "oldest" || ids[1] != "middle" || ids[2] != "newest" {
		t.Fatalf("listed %v, want oldest, middle, newest", ids)
	}
	alice.CancelAll(errCancelledByAgent)
}

// Shutdown ends a running background task without settling it: its record
// still says running and keeps its marker, so a restart relaunches it.
func TestShutdown_RunningTaskKeepsItsRecord(t *testing.T) {
	w := newBlockingWorker()
	mgr, reg, ws := newBlockingManager(t, w)
	fired := make(chan struct{}, 1)
	id, err := mgr.SpawnCallback("work", "job", "", "cli", "direct", "", "", nil,
		func(context.Context, *toolkit.Result) { fired <- struct{}{} }, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskCtx := <-w.started
	reg.Shutdown(nil)
	<-w.released
	if !errors.Is(context.Cause(taskCtx), hook.ErrShutdown) {
		t.Fatalf("task cause = %v, want shutdown", context.Cause(taskCtx))
	}
	rec, err := readStatus(tasksDirFor(ws), id)
	if err != nil || rec.Status != StatusRunning {
		t.Fatalf("record = %+v (%v), want still running", rec, err)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 1 || ids[0] != id {
		t.Fatalf("run markers = %v, want the task's", ids)
	}
	select {
	case <-fired:
		t.Fatal("a result was delivered for a task the shutdown ended")
	default:
	}
}

// Once the process shuts down no task starts: a background one keeps its
// record and marker for the restart; a waited one is cancelled, as its turn
// is replayed and starts it again.
func TestShutdown_TaskNotStarted(t *testing.T) {
	w := newBlockingWorker()
	mgr, reg, ws := newBlockingManager(t, w)
	reg.Shutdown(nil)

	id, err := mgr.SpawnCallback("work", "job", "", "cli", "direct", "", "", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := readStatus(tasksDirFor(ws), id)
	if err != nil || rec.Status != StatusRunning {
		t.Fatalf("background record = %+v (%v), want still running", rec, err)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 1 || ids[0] != id {
		t.Fatalf("run markers = %v, want the task's", ids)
	}

	res, err := mgr.Run(context.Background(), "do work", "waited", "", "cli", "direct", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.ForLLM, "was cancelled") {
		t.Fatalf("waited result = %+v, want cancelled", res)
	}
	select {
	case <-w.started:
		t.Fatal("a worker ran after the shutdown")
	default:
	}
}

// A relaunch whose id the registry already runs is refused: the task's
// record says so, as an error, and its marker goes.
func TestSupervise_DuplicateRelaunchIsAnError(t *testing.T) {
	w := newBlockingWorker()
	mgr, reg, ws := newBlockingManager(t, w)
	leftRecord(t, ws, "left")
	if _, _, err := TasksOf(reg, "alice").StartTask(context.Background(), hook.TaskStart{Info: hook.TaskInfo{ID: "left"}}); err != nil {
		t.Fatal(err)
	}
	mgr.SuperviseOnce(time.Now().Unix()+3600, nil)
	rec, err := readStatus(tasksDirFor(ws), "left")
	if err != nil || rec.Status != StatusError || !strings.Contains(rec.Error, "duplicate") {
		t.Fatalf("record = %+v (%v), want an error", rec, err)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 0 {
		t.Fatalf("run markers = %v, want none", ids)
	}
}
