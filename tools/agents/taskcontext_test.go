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
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// blockingWorker is a RunFull that runs until released, recording the
// context it ran on.
type blockingWorker struct {
	started  chan context.Context
	release  chan struct{}
	released chan struct{}
}

func newBlockingWorker() *blockingWorker {
	return &blockingWorker{
		started:  make(chan context.Context, 8),
		release:  make(chan struct{}),
		released: make(chan struct{}, 8),
	}
}

func (w *blockingWorker) run(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
	w.started <- ctx
	select {
	case <-w.release:
		return &global.SyncResult{Content: "worker output", Iterations: 1}, func() { w.released <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, func() { w.released <- struct{}{} }, context.Cause(ctx)
	}
}

func newBlockingManager(t *testing.T, w *blockingWorker) (*SubagentManager, *TaskRegistry, string) {
	t.Helper()
	ws := t.TempDir()
	reg := NewTaskRegistry()
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: ws, Live: NewLiveSet(), Tasks: reg, CallerAgentID: "alice", RunFull: w.run,
	})
	return mgr, reg, ws
}

// errInterrupted is a cause like the one /cancel gives a turn.
var errInterrupted = errors.New("interrupted by user")

// A wait task runs on a context of its own: ending the turn's context stops
// the waiting, not the task. The task becomes a callback task (record and
// marker), finishes and reports to the callback.
func TestRun_WaitTaskDetachesWhenTheTurnStopsWaiting(t *testing.T) {
	w := newBlockingWorker()
	mgr, reg, ws := newBlockingManager(t, w)
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	reported := make(chan *tools.ToolResult, 1)
	onDetach := func(_ context.Context, r *tools.ToolResult) { reported <- r }

	done := make(chan *tools.ToolResult, 1)
	go func() {
		res, err := mgr.Run(turnCtx, "do work", "job", "", "cli", "direct", "", "", nil, onDetach)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()
	taskCtx := <-w.started
	if TaskID(taskCtx) == "" {
		t.Fatal("the worker's context carries no task id")
	}
	if running := reg.Running("alice"); len(running) != 1 || running[0].Mode != hook.TaskWait {
		t.Fatalf("registry = %+v, want one wait task", running)
	}
	stopTurn(errInterrupted)
	res := <-done
	if !strings.Contains(res.ForLLM, "Stopped waiting") || res.IsError {
		t.Fatalf("result = %+v, want the detached notice", res)
	}
	if taskCtx.Err() != nil {
		t.Fatal("the worker's context was cancelled with the turn's")
	}
	id := TaskID(taskCtx)
	rec, err := readStatus(tasksDirFor(ws), id)
	if err != nil || rec.Mode != "callback" || rec.Status != StatusRunning {
		t.Fatalf("record = %+v (%v), want a running callback task", rec, err)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 1 {
		t.Fatalf("run markers = %v, want the detached task's", ids)
	}
	if running := reg.Running("alice"); len(running) != 1 || running[0].Mode != hook.TaskCallback {
		t.Fatalf("registry = %+v, want one callback task", running)
	}

	close(w.release)
	select {
	case r := <-reported:
		if !strings.Contains(r.ForLLM, id) || r.IsError {
			t.Fatalf("background result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the detached task's result was not delivered")
	}
	if rec, err = readStatus(tasksDirFor(ws), id); err != nil || rec.Status != StatusDone {
		t.Fatalf("status = %s, want done", rec.Status)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 0 {
		t.Fatalf("run markers left: %v", ids)
	}
	// The task left the registry before its result was delivered.
	if n := len(reg.Running("alice")); n != 0 {
		t.Fatalf("%d tasks still registered", n)
	}
}

// A turn shutdown stops does not detach its wait task: the task is
// cancelled (the turn is replayed on restart and starts it again) and is
// not relaunched.
func TestRun_WaitTaskCancelledOnShutdown(t *testing.T) {
	w := newBlockingWorker()
	mgr, reg, ws := newBlockingManager(t, w)
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	done := make(chan *tools.ToolResult, 1)
	go func() {
		res, err := mgr.Run(turnCtx, "do work", "job", "", "cli", "direct", "", "", nil, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()
	taskCtx := <-w.started
	stopTurn(hook.ErrShutdown)
	res := <-done
	if !res.IsError || !strings.Contains(res.ForLLM, "cancelled") {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	if !errors.Is(context.Cause(taskCtx), hook.ErrShutdown) {
		t.Fatalf("task cause = %v, want shutdown", context.Cause(taskCtx))
	}
	rec, err := readStatus(tasksDirFor(ws), TaskID(taskCtx))
	if err != nil || rec.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v), want cancelled", rec, err)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 0 {
		t.Fatalf("run markers = %v, want none", ids)
	}
	<-w.released
	if n := len(reg.Running("alice")); n != 0 {
		t.Fatalf("%d tasks still registered", n)
	}
}

// A task-model call waited for detaches the same way.
func TestRunTask_WaitDetaches(t *testing.T) {
	release := make(chan struct{})
	runner := &gatedTaskRunner{release: release, started: make(chan struct{}, 1)}
	reg := NewTaskRegistry()
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: t.TempDir(), Live: NewLiveSet(), Tasks: reg, CallerAgentID: "alice", TaskRunner: runner,
	})
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	reported := make(chan *tools.ToolResult, 1)
	done := make(chan *tools.ToolResult, 1)
	go func() {
		done <- mgr.RunTask(turnCtx, "sum", "cli", "direct", TaskModelRequest{Kind: TaskKindText, Prompt: "hi"},
			func(_ context.Context, r *tools.ToolResult) { reported <- r })
	}()
	<-runner.started
	stopTurn(errInterrupted)
	if res := <-done; !strings.Contains(res.ForLLM, "Stopped waiting") {
		t.Fatalf("result = %+v, want the detached notice", res)
	}
	if running := reg.Running("alice"); len(running) != 1 || running[0].Kind != TaskKindText {
		t.Fatalf("registry = %+v", running)
	}
	close(release)
	select {
	case r := <-reported:
		if !strings.Contains(r.ForLLM, "[TASK RESULT]") {
			t.Fatalf("background result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no background result")
	}
}

// gatedTaskRunner answers once released or its context ends.
type gatedTaskRunner struct {
	release chan struct{}
	started chan struct{}
}

func (g *gatedTaskRunner) Check(context.Context, string, string, TaskModelRequest) error { return nil }

func (g *gatedTaskRunner) Run(ctx context.Context, _, _ string, _ TaskModelRequest) (*TaskModelResult, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return &TaskModelResult{Model: "fast", Text: "summary"}, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// The registry lists an owner's running tasks (a clone's are its
// source's) and nobody else's.
func TestTaskRegistry_RunningByOwner(t *testing.T) {
	reg := NewTaskRegistry()
	_, a := reg.start(context.Background(), hook.TaskInfo{ID: "t1", Owner: "alice"}, 0, nil, nil)
	_, b := reg.start(context.Background(), hook.TaskInfo{ID: "t2", Owner: "bob"}, 0, nil, nil)
	if got := reg.Running("Alice"); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("Running(alice) = %+v", got)
	}
	if reg.Owns("alice", "t2") || !reg.Owns("bob", "t2") {
		t.Fatal("ownership is by owner")
	}
	a.finished(hook.TaskDone)
	b.finished(hook.TaskDone)
	if n := len(reg.Running("alice")) + len(reg.Running("bob")); n != 0 {
		t.Fatalf("%d tasks left", n)
	}
}

// A waited spawn whose turn has no single chat (several turns of the
// session run) cannot report later, as a callback spawn could not start
// there: when its turn stops waiting it is cancelled, and the model is told
// so, never that a result will arrive.
func TestSpawnTool_WaitInAmbiguousChatIsCancelledNotDetached(t *testing.T) {
	w := newBlockingWorker()
	mgr, _, ws := newBlockingManager(t, w)
	var spawn toolspec.ToolDefinition
	for _, d := range GlobalProvider.RegisterTools(toolspec.Deps{Spawn: NewSpawner(mgr)}) {
		if d.Name == "spawn" {
			spawn = d
		}
	}
	ctx, cancel := context.WithCancel(tools.WithReplyChat(context.Background(), tools.ReplyChat{Ambiguous: true}))
	notified := make(chan struct{}, 1)
	done := make(chan *toolspec.Result, 1)
	go func() {
		res, err := spawn.Handler(&toolspec.ToolCall{
			Ctx: ctx, Args: map[string]any{"task": "work", "mode": "wait"},
			Notify: func(*toolspec.Result) { notified <- struct{}{} },
		})
		if err != nil {
			t.Errorf("handler: %v", err)
		}
		done <- res
	}()
	taskCtx := <-w.started
	cancel()
	res := <-done
	if !res.IsError || !strings.Contains(res.ForLLM, "was cancelled") || strings.Contains(res.ForLLM, "background result") {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	<-w.released
	select {
	case <-notified:
		t.Fatal("a background result was sent")
	default:
	}
	if rec, err := readStatus(tasksDirFor(ws), TaskID(taskCtx)); err != nil || rec.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v)", rec, err)
	}
}
