// Agency
// License: MIT

package agents

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// originRun records the origin each worker ran with; a worker given change
// changes its billing code, as its billing tool would.
type originRun struct {
	mu      sync.Mutex
	origins []string
	change  string
}

func (o *originRun) run(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
	o.mu.Lock()
	o.origins = append(o.origins, tools.Origin(ctx))
	change := o.change
	o.mu.Unlock()
	if change != "" {
		tools.InheritedBillingOf(ctx).Set(change)
	}
	return &global.SyncResult{Content: "done"}, func() {}, nil
}

func (o *originRun) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.origins...)
}

// originManager is a manager whose spawning work has origin spawner's.
func originManager(t *testing.T, o *originRun, spawner func(context.Context) string) (*Spawner, *SubagentManager) {
	t.Helper()
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: t.TempDir(), Live: NewLiveSet(), CallerAgentID: "bob", RunFull: o.run, Origin: spawner,
	})
	return NewSpawner(mgr), mgr
}

// A task has the spawning work's origin, kept in its record; a billing code
// the task changes leaves it alone. A turn that inherited its origin
// records it for the task's result.
func TestSpawn_TaskOrigin(t *testing.T) {
	o := &originRun{change: "acme-2"}
	sp, mgr := originManager(t, o, func(context.Context) string { return "alice" })
	results := make(chan *toolspec.Result, 1)
	ctx := tools.WithOrigin(tools.WithBilling(context.Background(), "acme"), "alice")
	res := mustSpawn(t, sp, ctx, global.SpawnRequest{
		Mode: global.SpawnCallback, Task: "work", Name: "job",
		OnResult: func(r *toolspec.Result) { results <- r },
	})
	if res.IsError {
		t.Fatalf("Spawn: %s", res.ForLLM)
	}
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if got := o.seen(); len(got) != 1 || got[0] != "alice" {
		t.Errorf("worker had origin %v, want [alice]", got)
	}
	rec := onlyTask(t, mgr)
	if rec.Origin != "alice" || rec.ReplyOrigin != "alice" || rec.Billing != "acme-2" {
		t.Errorf("record origin %q, reply %q, billed %q; want alice, alice, acme-2", rec.Origin, rec.ReplyOrigin, rec.Billing)
	}

	// A turn with its agent's own origin hands that on but records no
	// reply origin.
	o2 := &originRun{}
	sp2, mgr2 := originManager(t, o2, func(context.Context) string { return "bob" })
	if res := mustSpawn(t, sp2, context.Background(), global.SpawnRequest{Mode: global.SpawnAndWait, Task: "work", Label: "job"}); res.IsError {
		t.Fatalf("Spawn: %s", res.ForLLM)
	}
	if rec := onlyTask(t, mgr2); rec.Origin != "bob" || rec.ReplyOrigin != "" {
		t.Errorf("record origin %q, reply %q; want bob and none", rec.Origin, rec.ReplyOrigin)
	}
}

// A task relaunched after a restart has the origin its record holds; a
// record written before origins has none, so its work has its own agent's.
func TestSupervise_RelaunchKeepsTheOrigin(t *testing.T) {
	for _, stored := range []string{"alice", ""} {
		o := &originRun{}
		_, mgr := originManager(t, o, nil)
		dir := mgr.tasksDir()
		rec := &TaskRecord{
			UUID: "t-" + stored, Name: "job", OwnerAgentID: "bob", Mode: "callback", Task: "work",
			Status: StatusRunning, ResultsPath: relResultsPath("t-" + stored), Origin: stored,
		}
		if err := writeStatus(dir, rec); err != nil {
			t.Fatal(err)
		}
		if err := markRun(dir, rec.UUID); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		mgr.SuperviseOnce(time.Now().Unix()+10, func(*TaskRecord) toolkit.AsyncCallback {
			return func(context.Context, *toolkit.Result) { close(done) }
		})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("relaunched task did not finish")
		}
		if got := o.seen(); len(got) != 1 || got[0] != stored {
			t.Errorf("stored %q: relaunch had origin %v", stored, got)
		}
	}
}

// Without the host's resolver a task takes the inherited origin; a
// task-model call records its origin like a sub-agent.
func TestSpawn_OriginWithoutResolver(t *testing.T) {
	_, mgr := originManager(t, &originRun{}, nil)
	if got := mgr.originOf(context.Background()).origin; got != "" {
		t.Errorf("no resolver, nothing inherited = %q", got)
	}
	if got := mgr.originOf(tools.WithOrigin(context.Background(), "alice")).origin; got != "alice" {
		t.Errorf("no resolver, inherited = %q", got)
	}

	runner := &originTaskRunner{}
	sp, _ := newTaskSpawner(t, runner)
	ctx := tools.WithOrigin(context.Background(), "alice")
	if res := mustSpawn(t, sp, ctx, global.SpawnRequest{Mode: global.SpawnAndWait, Kind: "text", Task: "hi"}); res.IsError {
		t.Fatalf("Spawn: %s", res.ForLLM)
	}
	if runner.origin != "alice" {
		t.Errorf("task model had origin %q, want alice", runner.origin)
	}
}

// originTaskRunner records the origin a task-model call runs with.
type originTaskRunner struct{ origin string }

func (r *originTaskRunner) Check(context.Context, string, string, TaskModelRequest) error {
	return nil
}

func (r *originTaskRunner) Run(ctx context.Context, _, _ string, _ TaskModelRequest) (*TaskModelResult, error) {
	r.origin = tools.Origin(ctx)
	return &TaskModelResult{Model: "fast", Text: "ok"}, nil
}
