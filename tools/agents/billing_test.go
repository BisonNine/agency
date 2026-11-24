// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package agents

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// billingRun records the billing code each worker ran under; a worker given
// change sets its own code to it, as its billing tool would.
type billingRun struct {
	mu     sync.Mutex
	codes  []string
	change string
}

func (b *billingRun) run(ctx context.Context, _, _, _, _ string, _ []string) (*global.SyncResult, func(), error) {
	code, _ := tools.Billing(ctx)
	b.mu.Lock()
	b.codes = append(b.codes, code)
	change := b.change
	b.mu.Unlock()
	if change != "" {
		tools.InheritedBillingOf(ctx).Set(change)
	}
	return &global.SyncResult{Content: "done"}, func() {}, nil
}

func (b *billingRun) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.codes...)
}

// billingManager is a manager whose spawning work is billed to spawner.
func billingManager(t *testing.T, b *billingRun, spawner func(context.Context) string) (*Spawner, *SubagentManager) {
	t.Helper()
	mgr := NewSubagentManager(SubagentManagerConfig{
		Workspace: t.TempDir(), Live: NewLiveSet(), CallerAgentID: "alice", RunFull: b.run, Billing: spawner,
	})
	return NewSpawner(mgr), mgr
}

// onlyTask is the one task record of mgr.
func onlyTask(t *testing.T, mgr *SubagentManager) *TaskRecord {
	t.Helper()
	list, err := mgr.TaskList()
	if err != nil || len(list) != 1 {
		t.Fatalf("TaskList = %+v, %v; want one task", list, err)
	}
	rec, err := readStatus(mgr.tasksDir(), list[0].UUID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A task is billed to the spawn's billing argument, else to the spawning
// work's code, and its record keeps the code.
func TestSpawn_TaskBilling(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, arg, want string
	}{
		{"spawner's code", "", "garden"},
		{"argument", "acme", "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &billingRun{}
			sp, mgr := billingManager(t, b, func(context.Context) string { return "garden" })
			res := mustSpawn(t, sp, ctx, global.SpawnRequest{Mode: global.SpawnAndWait, Task: "work", Label: "job", Billing: tc.arg})
			if res.IsError {
				t.Fatalf("Spawn: %s", res.ForLLM)
			}
			if got := b.seen(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("worker billed %v, want [%s]", got, tc.want)
			}
			if rec := onlyTask(t, mgr); rec.Billing != tc.want || rec.ReplyBilling != "" {
				t.Errorf("record billed %q (reply %q), want %q", rec.Billing, rec.ReplyBilling, tc.want)
			}
		})
	}
}

// A background task started from a turn that inherited its code records
// that code for its result, and a change the task makes is written to its
// record, so a relaunch keeps it.
func TestSpawn_TaskChangesItsOwnCode(t *testing.T) {
	b := &billingRun{change: "acme-2"}
	sp, mgr := billingManager(t, b, nil)
	results := make(chan *toolspec.Result, 1)
	ctx := tools.WithBilling(context.Background(), "acme")
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
	if got := b.seen(); len(got) != 1 || got[0] != "acme" {
		t.Errorf("worker billed %v, want [acme]", got)
	}
	rec := onlyTask(t, mgr)
	if rec.Billing != "acme-2" || rec.ReplyBilling != "acme" {
		t.Errorf("record billed %q, reply %q; want acme-2 and acme", rec.Billing, rec.ReplyBilling)
	}
	if code, _ := tools.Billing(ctx); code != "acme" {
		t.Errorf("the spawning turn's code = %q, want acme kept", code)
	}
}

// A relaunched task runs under the code its record holds; a record written
// before billing codes runs under the default.
func TestSupervise_RelaunchKeepsTheCode(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{{"acme-2", "acme-2"}, {"", global.DefaultBilling}, {"not a code", global.DefaultBilling}} {
		b := &billingRun{}
		_, mgr := billingManager(t, b, nil)
		dir := mgr.tasksDir()
		rec := &TaskRecord{
			UUID: "t-" + tc.want, Name: "job", OwnerAgentID: "alice", Mode: "callback", Task: "work",
			Status: StatusRunning, ResultsPath: relResultsPath("t-" + tc.want), Billing: tc.stored,
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
		if got := b.seen(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("stored %q: relaunch billed %v, want [%s]", tc.stored, got, tc.want)
		}
	}
}

// An invalid billing argument is refused before anything starts.
func TestSpawn_InvalidBilling(t *testing.T) {
	b := &billingRun{}
	sp, mgr := billingManager(t, b, nil)
	res := mustSpawn(t, sp, context.Background(), global.SpawnRequest{Mode: global.SpawnAndWait, Task: "work", Billing: "a b"})
	if !res.IsError || res.ForLLM != invalidBillingText {
		t.Fatalf("result = %+v, want the invalid billing text", res)
	}
	if list, err := mgr.TaskList(); err != nil || len(list) != 0 || len(b.seen()) != 0 {
		t.Errorf("a refused spawn started %d tasks (%v)", len(list), err)
	}
	// The tool refuses a billing argument that is not a string.
	defs := GlobalProvider.RegisterTools(toolspec.Deps{Spawn: sp})
	out, err := defs[0].Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"task": "work", "billing": 7.0}})
	if err != nil || !out.IsError || out.ForLLM != invalidBillingText {
		t.Errorf("non-string billing = %+v, %v", out, err)
	}
	props, ok := spawnToolSchema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("agent_spawn's schema has no properties")
	}
	arg, ok := props["billing"].(map[string]any)
	if desc, isString := arg["description"].(string); !ok || !isString || !strings.Contains(desc, "billing code") {
		t.Error("agent_spawn has no billing argument")
	}
}

// Without the host's resolver a task takes the inherited code, else the
// default; a task-model call is billed like a sub-agent.
func TestSpawn_BillingWithoutResolver(t *testing.T) {
	_, mgr := billingManager(t, &billingRun{}, nil)
	if got := mgr.billingOf(context.Background()); got != global.DefaultBilling {
		t.Errorf("no resolver, nothing inherited = %q", got)
	}
	if got := mgr.billingOf(tools.WithBilling(context.Background(), "acme")); got != "acme" {
		t.Errorf("no resolver, inherited = %q", got)
	}

	runner := &billingTaskRunner{}
	sp, _ := newTaskSpawner(t, runner)
	res := mustSpawn(t, sp, context.Background(), global.SpawnRequest{Mode: global.SpawnAndWait, Kind: "text", Task: "hi", Billing: "acme"})
	if res.IsError {
		t.Fatalf("Spawn: %s", res.ForLLM)
	}
	if runner.code != "acme" {
		t.Errorf("task model billed %q, want acme", runner.code)
	}
}

// billingTaskRunner records the code a task-model call runs under.
type billingTaskRunner struct{ code string }

func (r *billingTaskRunner) Check(context.Context, string, string, TaskModelRequest) error {
	return nil
}

func (r *billingTaskRunner) Run(ctx context.Context, _, _ string, _ TaskModelRequest) (*TaskModelResult, error) {
	r.code, _ = tools.Billing(ctx)
	return &TaskModelResult{Model: "fast", Text: "ok"}, nil
}
