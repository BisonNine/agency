// ClawEh
// License: MIT

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// readResults reads a task's results file.
func readResults(dir, id string) (*TaskResults, error) {
	b, err := os.ReadFile(resultsPath(dir, id))
	if err != nil {
		return nil, err
	}
	var r TaskResults
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// cancelFixture is Alice's manager over a worker that runs until its
// context ends, reporting what it saw then.
type cancelFixture struct {
	mgr     *SubagentManager
	reg     *TaskRegistry
	live    *LiveSet
	ws      string
	started chan context.Context
	// atCancel receives, for each cancelled worker, whether its run marker
	// was already gone and its status cancelled when its context ended.
	atCancel chan bool
	partial  string
	// released receives once for each worker whose clone was released.
	released chan struct{}
}

func newCancelFixture(t *testing.T) *cancelFixture {
	t.Helper()
	f := &cancelFixture{
		reg: NewTaskRegistry(), live: NewLiveSet(), ws: t.TempDir(),
		started: make(chan context.Context, 8), atCancel: make(chan bool, 8), released: make(chan struct{}, 8),
	}
	f.mgr = NewSubagentManager(SubagentManagerConfig{
		Workspace: f.ws, Live: f.live, Tasks: f.reg, CallerAgentID: "alice",
		RunFull: func(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
			f.started <- ctx
			<-ctx.Done()
			id := TaskID(ctx)
			rec, err := readStatus(tasksDirFor(f.ws), id)
			f.atCancel <- err == nil && rec.Status == StatusCancelled && !slices.Contains(listRunUUIDs(tasksDirFor(f.ws)), id) && !f.live.Has(id)
			return &global.SyncResult{Content: f.partial}, func() { f.released <- struct{}{} }, context.Cause(ctx)
		},
	})
	return f
}

func (f *cancelFixture) spawn(t *testing.T, ctx context.Context, cb tools.AsyncCallback) (string, context.Context) {
	t.Helper()
	id, err := f.mgr.spawnCallback(ctx, originOf(ctx), "work", "job", "", "cli", "direct", "", "", nil, cb)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case wctx := <-f.started:
		return id, wctx
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not start")
		return "", nil
	}
}

// Rule 5.3: agent_cancel cancels the task's context with its cause; the
// record says cancelled and the marker and live entry are gone before the
// context is; no callback fires; the partial output is kept, marked
// incomplete; a second cancel reports it finished.
func TestCancelTask_SettlesBeforeCancelling(t *testing.T) {
	f := newCancelFixture(t)
	f.partial = "half of it"
	fired := make(chan struct{}, 1)
	id, wctx := f.spawn(t, context.Background(), func(context.Context, *tools.ToolResult) { fired <- struct{}{} })

	res := f.mgr.CancelTask(id)
	if res.Cancelled != 1 || len(res.Tasks) != 1 || res.Tasks[0].UUID != id || res.Tasks[0].Status != StatusCancelled {
		t.Fatalf("result = %+v, want the task cancelled", res)
	}
	if !<-f.atCancel {
		t.Fatal("the context was cancelled before the record was settled")
	}
	if !errors.Is(context.Cause(wctx), hook.ErrInterrupted) {
		t.Fatalf("cause = %v, want an interrupt", context.Cause(wctx))
	}
	// Once the worker is gone its partial output is recorded, and nothing
	// more can be delivered.
	<-f.released
	r, err := readResults(tasksDirFor(f.ws), id)
	if err != nil || r.Status != StatusCancelled || !r.Incomplete || r.Content != "half of it" {
		t.Fatalf("results = %+v (%v), want the partial output, cancelled and incomplete", r, err)
	}
	select {
	case <-fired:
		t.Fatal("a cancelled task's callback fired")
	default:
	}
	if st, err := f.mgr.TaskStatus(id); err != nil || st.Status != StatusCancelled {
		t.Fatalf("agent_status = %+v (%v), want cancelled", st, err)
	}
	again := f.mgr.CancelTask(id)
	if again.Cancelled != 0 || len(again.AlreadyFinished) != 1 || again.AlreadyFinished[0].Status != StatusCancelled {
		t.Fatalf("second cancel = %+v, want already finished", again)
	}
}

// A task's own tasks are cancelled with it, first.
func TestCancelTask_Cascades(t *testing.T) {
	f := newCancelFixture(t)
	parent, pctx := f.spawn(t, context.Background(), nil)
	child, cctx := f.spawn(t, pctx, nil) // started from inside the parent's worker
	other, octx := f.spawn(t, context.Background(), nil)

	res := f.mgr.CancelTask(parent)
	if res.Cancelled != 2 || res.Tasks[0].UUID != child || res.Tasks[1].UUID != parent {
		t.Fatalf("result = %+v, want the child then the parent", res)
	}
	if pctx.Err() == nil || cctx.Err() == nil || octx.Err() != nil {
		t.Fatal("cancel reached the wrong tasks")
	}
	if running := f.reg.Running("alice"); len(running) != 1 || running[0].ID != other {
		t.Fatalf("running = %+v, want the unrelated task", running)
	}
}

// A cancelled task is not relaunched by the supervisor after a restart.
func TestCancelTask_NotRelaunchedAfterRestart(t *testing.T) {
	f := newCancelFixture(t)
	id, _ := f.spawn(t, context.Background(), nil)
	f.mgr.CancelTask(id)
	<-f.atCancel

	// A restart: a fresh live set and registry over the same workspace.
	live, reg := NewLiveSet(), NewTaskRegistry()
	restarted := NewSubagentManager(SubagentManagerConfig{
		Workspace: f.ws, Live: live, Tasks: reg, CallerAgentID: "alice",
		RunFull: func(context.Context, string, string, string, string, []string) (*global.SyncResult, func(), error) {
			return &global.SyncResult{}, func() {}, nil
		},
	})
	restarted.SuperviseOnce(time.Now().Unix()+3600, nil)
	// A relaunch registers the task before its worker starts.
	if live.Has(id) || len(reg.Running("alice")) != 0 {
		t.Fatal("the cancelled task was relaunched")
	}
}

// A task a restart left for the supervisor is settled as cancelled and
// never relaunched; a finished one is reported finished; another agent's
// task, and no task at all, are unknown.
func TestCancelTask_RecordsOnDisk(t *testing.T) {
	f := newCancelFixture(t)
	dir := tasksDirFor(f.ws)
	left := &TaskRecord{UUID: "left", Name: "left", OwnerAgentID: "alice", Mode: "callback", Status: StatusRunning, ResultsPath: relResultsPath("left")}
	done := &TaskRecord{UUID: "done", Name: "done", OwnerAgentID: "alice", Mode: "callback", Status: StatusDone}
	bobs := &TaskRecord{UUID: "bobs", Name: "bobs", OwnerAgentID: "bob", Mode: "callback", Status: StatusRunning}
	for _, rec := range []*TaskRecord{left, done, bobs} {
		if err := writeStatus(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := markRun(dir, "left"); err != nil {
		t.Fatal(err)
	}
	if res := f.mgr.CancelTask("left"); res.Cancelled != 1 {
		t.Fatalf("left = %+v", res)
	}
	if ids := listRunUUIDs(dir); len(ids) != 0 {
		t.Fatalf("markers left: %v", ids)
	}
	if res := f.mgr.CancelTask("done"); len(res.AlreadyFinished) != 1 || res.AlreadyFinished[0].Status != StatusDone {
		t.Fatalf("done = %+v", res)
	}
	for _, id := range []string{"bobs", "nope"} {
		if res := f.mgr.CancelTask(id); !slices.Equal(res.Unknown, []string{id}) || res.Cancelled != 0 {
			t.Fatalf("%s = %+v, want unknown", id, res)
		}
	}
}

// all cancels every running task of the agent and nobody else's; a turn
// still waiting for one of them is told it was cancelled. Task-model jobs
// are cancelled the same way.
func TestCancelAllTasks(t *testing.T) {
	f := newCancelFixture(t)
	runner := &gatedTaskRunner{release: make(chan struct{}), started: make(chan struct{}, 1)}
	f.mgr.taskRunner = runner
	bob := NewSubagentManager(SubagentManagerConfig{
		Workspace: t.TempDir(), Live: f.live, Tasks: f.reg, CallerAgentID: "bob",
		RunFull: func(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
			<-ctx.Done()
			return nil, func() {}, ctx.Err()
		},
	})
	if _, err := bob.SpawnCallback("bob's", "b", "", "cli", "direct", "", "", nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	f.spawn(t, context.Background(), nil)
	if _, err := f.mgr.SpawnTaskCallback("speak", "cli", "direct", TaskModelRequest{Kind: TaskKindTTS, Prompt: "hi"}, nil, 0); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	waited := make(chan *tools.ToolResult, 1)
	go func() {
		res, err := f.mgr.Run(context.Background(), "wait for me", "w", "", "cli", "direct", "", "", nil, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		waited <- res
	}()
	<-f.started

	res := f.mgr.CancelAllTasks()
	if res.Cancelled != 3 {
		t.Fatalf("result = %+v, want 3 cancelled", res)
	}
	if got := <-waited; !got.IsError || !strings.Contains(got.ForLLM, "cancelled") {
		t.Fatalf("the waiting turn got %+v", got)
	}
	if len(f.reg.Running("alice")) != 0 || len(f.reg.Running("bob")) != 1 {
		t.Fatalf("alice %+v, bob %+v", f.reg.Running("alice"), f.reg.Running("bob"))
	}
	f.reg.CancelAll("bob", errCancelledByAgent)
}

// The tool: one task by uuid or all, never both nor neither; the result is
// JSON for the model.
func TestCancelTool(t *testing.T) {
	f := newCancelFixture(t)
	sp := NewSpawner(f.mgr)
	var cancel global.ToolDefinition
	deps := global.Deps{Spawn: sp, Host: tools.ToolDeps{Cfg: &config.Config{}}}
	for _, d := range GlobalProvider.RegisterTools(deps) {
		if d.Name == "cancel" {
			cancel = d
		}
	}
	if cancel.Handler == nil {
		t.Fatal("agent_cancel is not built")
	}
	call := func(args map[string]any) *global.Result {
		t.Helper()
		res, err := cancel.Handler(&global.ToolCall{Ctx: context.Background(), Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, args := range []map[string]any{{}, {"uuid": "x", "all": true}} {
		if res := call(args); !res.IsError {
			t.Fatalf("%v accepted: %+v", args, res)
		}
	}
	id, _ := f.spawn(t, context.Background(), nil)
	var out global.TaskCancelResult
	if err := json.Unmarshal([]byte(call(map[string]any{"all": true}).ForLLM), &out); err != nil {
		t.Fatal(err)
	}
	if out.Cancelled != 1 || out.Tasks[0].UUID != id {
		t.Fatalf("all = %+v", out)
	}
	if err := json.Unmarshal([]byte(call(map[string]any{"uuid": id}).ForLLM), &out); err != nil {
		t.Fatal(err)
	}
	if out.Cancelled != 0 || len(out.AlreadyFinished) != 1 {
		t.Fatalf("again = %+v", out)
	}
}

// agent_cancel is never in the catalogue's tool list: it comes with
// agent_spawn.
func TestCancelTool_NotEnumerated(t *testing.T) {
	for _, d := range GlobalProvider.RegisterTools(global.Deps{}) {
		if d.Name == "cancel" {
			t.Fatal("agent_cancel enumerated without an agent")
		}
	}
}

// A wait task that ends before its turn starts waiting still hands its
// result to the turn.
func TestRun_WaitTaskEndingAtOnce(t *testing.T) {
	runner := &fakeTaskRunner{res: &TaskModelResult{Model: "fast", Text: "ok"}}
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: t.TempDir(), Live: NewLiveSet(), CallerAgentID: "alice", TaskRunner: runner,
	})
	for range 200 {
		done := make(chan *tools.ToolResult, 1)
		go func() {
			done <- mgr.RunTask(context.Background(), "t", "cli", "direct", TaskModelRequest{Kind: TaskKindText, Prompt: "hi"}, nil)
		}()
		select {
		case res := <-done:
			if res.IsError {
				t.Fatalf("result = %+v", res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the waiting turn never got the result")
		}
	}
}

// leftRecord writes a running record with its marker that no worker runs,
// as a restart leaves it for the supervisor.
func leftRecord(t *testing.T, ws, id string) {
	t.Helper()
	dir := tasksDirFor(ws)
	rec := &TaskRecord{UUID: id, Name: id, OwnerAgentID: "alice", Mode: "callback", Task: "work", Status: StatusRunning, ResultsPath: relResultsPath(id)}
	if err := writeStatus(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := markRun(dir, id); err != nil {
		t.Fatal(err)
	}
}

// A cancel and the supervisor never let a cancelled task run again: not
// when the supervisor listed the task's marker before the cancel, nor when
// it looks right after the cancel settled the task.
func TestCancel_SupervisorDoesNotRelaunchACancelledTask(t *testing.T) {
	future := time.Now().Unix() + 3600
	for _, order := range []string{"listed first", "settled first"} {
		t.Run(order, func(t *testing.T) {
			f := newCancelFixture(t)
			id, _ := f.spawn(t, context.Background(), nil)
			if order == "listed first" {
				f.mgr.hooks.listed = func(listed string) {
					if listed == id {
						f.mgr.CancelTask(id)
					}
				}
				f.mgr.SuperviseOnce(future, nil)
			} else {
				f.mgr.hooks.settled = func(string) { f.mgr.SuperviseOnce(future, nil) }
				f.mgr.CancelTask(id)
			}
			<-f.atCancel
			select {
			case <-f.started:
				t.Fatal("the cancelled task was relaunched")
			default:
			}
			if f.live.Has(id) || len(f.reg.Running("alice")) != 0 {
				t.Fatal("the cancelled task is running again")
			}
			if rec, err := readStatus(tasksDirFor(f.ws), id); err != nil || rec.Status != StatusCancelled {
				t.Fatalf("record = %+v (%v)", rec, err)
			}
		})
	}
}

// A relaunch, by another manager of the same agent, landing between
// agent_cancel's two looks does not make it report a running task as
// finished: the relaunched task is cancelled.
func TestCancelTask_RelaunchedMeanwhileIsCancelled(t *testing.T) {
	f := newCancelFixture(t)
	leftRecord(t, f.ws, "left")
	other := NewSubagentManager(SubagentManagerConfig{
		Workspace: f.ws, Live: f.live, Tasks: f.reg, CallerAgentID: "alice", RunFull: f.mgr.runFull,
	})
	f.mgr.hooks.beforeRecordCancel = func(string) {
		other.SuperviseOnce(time.Now().Unix()+3600, nil)
		<-f.started
	}
	res := f.mgr.CancelTask("left")
	if res.Cancelled != 1 || len(res.AlreadyFinished) != 0 {
		t.Fatalf("result = %+v, want the relaunched task cancelled", res)
	}
	<-f.atCancel
	if rec, err := readStatus(tasksDirFor(f.ws), "left"); err != nil || rec.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v)", rec, err)
	}
}

// Cancelling a task a restart left behind cancels the tasks it started,
// which were relaunched, first.
func TestCancelTask_LeftBehindCascades(t *testing.T) {
	f := newCancelFixture(t)
	leftRecord(t, f.ws, "left")
	child, cctx := f.spawn(t, WithTaskID(context.Background(), "left"), nil)
	res := f.mgr.CancelTask("left")
	if res.Cancelled != 2 || res.Tasks[0].UUID != child || res.Tasks[1].UUID != "left" {
		t.Fatalf("result = %+v, want the child then the task", res)
	}
	if cctx.Err() == nil {
		t.Fatal("the child is still running")
	}
}

// A task started under a task being cancelled does not escape the cascade.
func TestCancelTask_ChildStartedLateIsCancelled(t *testing.T) {
	f := newCancelFixture(t)
	parent, pctx := f.spawn(t, context.Background(), nil)
	f.mgr.CancelTask(parent)
	<-f.atCancel
	events := make(chan hook.TaskInfo, 8)
	f.reg.Observe(func(info hook.TaskInfo) { events <- info })
	id, err := f.mgr.spawnCallback(pctx, originOf(pctx), "work", "job", "", "cli", "direct", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Told twice it is cancelled: as it starts, and once its worker is gone.
	for n := 0; n < 2; {
		if info := <-events; info.ID == id && info.State == hook.TaskCancelled {
			n++
		}
	}
	select {
	case <-f.started:
		t.Fatal("a task started under a cancelled task ran")
	default:
	}
	if rec, err := readStatus(tasksDirFor(f.ws), id); err != nil || rec.Status != StatusCancelled || rec.StartedAt != "" {
		t.Fatalf("record = %+v (%v)", rec, err)
	}
}

// A wait task whose turn stops waiting while the process shuts down is
// cancelled, whatever the turn's context's cause (a CLI's MCP request
// ends with a plain cancel), so it is not relaunched beside the replayed
// turn.
func TestRun_WaitTaskCancelledWhileShuttingDown(t *testing.T) {
	w := newBlockingWorker()
	ws := t.TempDir()
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: ws, Live: NewLiveSet(), CallerAgentID: "alice", RunFull: w.run,
		ShuttingDown: func() bool { return true },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *tools.ToolResult, 1)
	go func() {
		res, err := mgr.Run(ctx, "do work", "job", "", "cli", "direct", "", "", nil, func(context.Context, *tools.ToolResult) {
			t.Error("a background result was delivered")
		})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()
	taskCtx := <-w.started
	cancel()
	if res := <-done; !res.IsError || !strings.Contains(res.ForLLM, "cancelled") {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	if ids := listRunUUIDs(tasksDirFor(ws)); len(ids) != 0 {
		t.Fatalf("run markers = %v, want none", ids)
	}
	if rec, err := readStatus(tasksDirFor(ws), TaskID(taskCtx)); err != nil || rec.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v)", rec, err)
	}
}

// The supervisor relaunching a task whose parent was cancelled cancels it
// at once, without waiting on itself.
func TestSupervise_OrphanRelaunchIsCancelled(t *testing.T) {
	f := newCancelFixture(t)
	dir := tasksDirFor(f.ws)
	rec := &TaskRecord{UUID: "child", Name: "child", OwnerAgentID: "alice", Mode: "callback", Task: "work", Status: StatusRunning, Parent: "gone", ResultsPath: relResultsPath("child")}
	if err := writeStatus(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := markRun(dir, "child"); err != nil {
		t.Fatal(err)
	}
	f.reg.CancelChildren("gone", errCancelledByAgent)
	done := make(chan struct{})
	go func() {
		f.mgr.SuperviseOnce(time.Now().Unix()+3600, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor deadlocked")
	}
	if got, err := readStatus(dir, "child"); err != nil || got.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v)", got, err)
	}
}

// The supervisor decides a relaunch, and a cancel settles a record, under
// the settle lock every manager shares; a cancel writes cancelled before it
// clears the marker.
func TestSettleLockHeldWhereItMatters(t *testing.T) {
	f := newCancelFixture(t)
	leftRecord(t, f.ws, "left")
	held := func(where string) {
		if f.live.settle.TryLock() {
			f.live.settle.Unlock()
			t.Errorf("%s: the settle lock is not held", where)
		}
	}
	f.mgr.hooks.decided = func(string) { held("supervisor decision") }
	f.mgr.SuperviseOnce(time.Now().Unix()+3600, nil)
	<-f.started

	f.mgr.hooks.settling = func(id string) {
		held("cancel settling")
		if rec, err := readStatus(tasksDirFor(f.ws), id); err != nil || rec.Status != StatusCancelled {
			t.Errorf("record = %+v (%v): the marker would go before the record says cancelled", rec, err)
		}
		if !slices.Contains(listRunUUIDs(tasksDirFor(f.ws)), id) {
			t.Error("the marker was cleared before the record said cancelled")
		}
	}
	f.mgr.CancelTask("left")
	<-f.atCancel
}

// A child a restart left waiting for its relaunch is cancelled, not
// relaunched, once its parent's record says cancelled.
func TestSupervise_ChildOfCancelledParentIsNotRelaunched(t *testing.T) {
	f := newCancelFixture(t)
	dir := tasksDirFor(f.ws)
	parent := &TaskRecord{UUID: "p", Name: "p", OwnerAgentID: "alice", Mode: "callback", Status: StatusCancelled}
	child := &TaskRecord{UUID: "c", Name: "c", OwnerAgentID: "alice", Mode: "callback", Task: "work", Status: StatusRunning, Parent: "p", ResultsPath: relResultsPath("c")}
	for _, rec := range []*TaskRecord{parent, child} {
		if err := writeStatus(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := markRun(dir, "c"); err != nil {
		t.Fatal(err)
	}
	f.mgr.SuperviseOnce(time.Now().Unix()+3600, nil)
	if f.live.Has("c") {
		t.Fatal("the child was relaunched")
	}
	if rec, err := readStatus(dir, "c"); err != nil || rec.Status != StatusCancelled {
		t.Fatalf("record = %+v (%v)", rec, err)
	}
}

// A task started under a task of a cascade after the cascade took its
// snapshot, before that task itself was stopped, is cancelled too.
func TestTaskRegistry_NothingEscapesACascade(t *testing.T) {
	reg := NewTaskRegistry()
	reg.grace = time.Nanosecond // these tasks have no worker to wait for
	var late *Task
	_, _ = reg.start(context.Background(), hook.TaskInfo{ID: "r", Owner: "alice"}, 0, nil, nil)
	_, _ = reg.start(context.Background(), hook.TaskInfo{ID: "c", Owner: "alice", Parent: "r"}, 0, nil, func(error) bool {
		// While the child is being cancelled, the root starts another.
		_, late = reg.start(context.Background(), hook.TaskInfo{ID: "late", Owner: "alice", Parent: "r"}, 0, nil, nil)
		return true
	})
	reg.Cancel("alice", "r", errCancelledByAgent)
	if late == nil || late.Info().State != hook.TaskCancelled {
		t.Fatal("a task started during the cascade escaped it")
	}
	if n := len(reg.Running("alice")); n != 0 {
		t.Fatalf("%d tasks still running", n)
	}
}

// A cancel losing to the task's own end leaves the end event to it.
func TestTaskRegistry_EndEventSurvivesALostCancel(t *testing.T) {
	reg := NewTaskRegistry()
	var last hook.TaskInfo
	reg.Observe(func(info hook.TaskInfo) { last = info })
	_, task := reg.start(context.Background(), hook.TaskInfo{ID: "t", Owner: "alice"}, 0, nil, func(error) bool { return false })
	if _, stopped := task.stop(errCancelledByAgent); stopped {
		t.Fatal("an ended task reported as cancelled")
	}
	task.finished(hook.TaskDone)
	if last.State != hook.TaskDone {
		t.Fatalf("last event = %+v, want done", last)
	}
}

// Once agent_cancel returns, the task's record and results file are final:
// the worker's partial output is already in, and nothing is written later.
// A worker that outlives the cancel's wait is sealed and writes nothing.
func TestCancelTask_RecordFinalWhenCancelReturns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stuck   bool
		partial string
	}{
		{"worker returns at once", false, "half of it"},
		{"worker outlives the wait", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCancelFixture(t)
			if tc.stuck {
				f.reg.grace = time.Millisecond // the wait gives up
			} else {
				f.reg.grace = time.Minute // never reached: the worker returns
			}
			release := make(chan struct{})
			exited := make(chan struct{})
			f.mgr.runFull = func(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
				f.started <- ctx
				<-ctx.Done()
				if tc.stuck {
					<-release
				}
				return &global.SyncResult{Content: "half of it"}, func() { close(exited) }, context.Cause(ctx)
			}
			id, _ := f.spawn(t, context.Background(), nil)
			f.mgr.CancelTask(id)
			dir := tasksDirFor(f.ws)
			snapshot := func() string {
				st, err := os.ReadFile(statusPath(dir, id))
				if err != nil {
					t.Fatal(err)
				}
				rs, err := os.ReadFile(resultsPath(dir, id))
				if err != nil {
					t.Fatal(err)
				}
				return string(st) + string(rs)
			}
			before := snapshot()
			r, err := readResults(dir, id)
			if err != nil || r.Status != StatusCancelled || r.Content != tc.partial {
				t.Fatalf("results when the cancel returned = %+v (%v), want %q", r, err, tc.partial)
			}
			close(release)
			<-exited
			if after := snapshot(); after != before {
				t.Fatalf("the task's files changed after the cancel returned:\n%s\n---\n%s", before, after)
			}
		})
	}
}
