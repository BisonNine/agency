// ClawEh
// License: MIT

package agents

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// TaskRegistry is the process's running tasks: every sub-agent and
// task-model job, each on a context of its own, registered under its id
// with the agent that owns it (a clone's tasks are its source's). A task is
// not a child of the turn that started it: stopping the turn does not reach
// it, and it is cancelled by id (Cancel) or with everything its owner runs
// (CancelAll). One registry is shared by every SubagentManager of the
// process, across config reloads.
type TaskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

// NewTaskRegistry returns an empty registry.
func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{tasks: make(map[string]*Task)}
}

// Task is one registered task.
type Task struct {
	reg    *TaskRegistry
	cancel context.CancelCauseFunc
	// beforeCancel runs once, before the task's context is cancelled: it
	// takes the task out of what the supervisor and a restart relaunch. It
	// reports false for a task that had already ended.
	beforeCancel func(cause error) bool

	mu   sync.Mutex
	info hook.TaskInfo
	once sync.Once
}

// Info is the task's description now.
func (t *Task) Info() hook.TaskInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.info
}

// setMode records how the task's owner now receives its result.
func (t *Task) setMode(m hook.TaskMode) {
	t.mu.Lock()
	t.info.Mode = m
	t.mu.Unlock()
}

// stop cancels the task with cause, once: its record is settled first
// (beforeCancel), then it leaves the registry, then its context is
// cancelled. It reports whether this call cancelled it: false for a task
// that had already ended (its result is being delivered).
func (t *Task) stop(cause error) (hook.TaskInfo, bool) {
	stopped := false
	t.once.Do(func() {
		stopped = t.beforeCancel == nil || t.beforeCancel(cause)
		if stopped {
			t.mu.Lock()
			t.info.State, t.info.Cause, t.info.Ended = hook.TaskCancelled, cause, time.Now()
			t.mu.Unlock()
		}
		t.reg.remove(t.info.ID)
		t.cancel(cause)
	})
	return t.Info(), stopped
}

// finished takes an ended task out of the registry and releases its context.
func (t *Task) finished() {
	t.once.Do(func() {
		t.reg.remove(t.info.ID)
		t.cancel(nil)
	})
}

type taskIDKey struct{}

// WithTaskID returns ctx carrying the id of the task it runs in, so a task
// started from it is recorded as that task's own (cascading cancels).
func WithTaskID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskID is the id of the task ctx runs in; empty outside a task.
func TaskID(ctx context.Context) string {
	if id, ok := ctx.Value(taskIDKey{}).(string); ok {
		return id
	}
	return ""
}

// isolatedContext is a context with nothing of its parent: no deadline, no
// cancellation and no values.
type isolatedContext struct{ context.Context }

func (isolatedContext) Value(any) any { return nil }

// start registers a task described by info and returns the context it runs
// on: nothing of parent's (the turn that started it, or none) but the
// sub-agent depth and ask chain it is given and the task's own id.
// beforeCancel is run by a cancel before the context is cancelled.
func (r *TaskRegistry) start(parent context.Context, info hook.TaskInfo, depth int, askChain []string, beforeCancel func(error) bool) (context.Context, *Task) {
	ctx := WithSpawnDepth(isolatedContext{context.WithoutCancel(parent)}, depth)
	if len(askChain) > 0 {
		ctx = tools.WithAskChain(ctx, askChain)
	}
	ctx, cancel := context.WithCancelCause(WithTaskID(ctx, info.ID))
	info.State = hook.TaskRunning
	if info.Started.IsZero() {
		info.Started = time.Now()
	}
	t := &Task{reg: r, cancel: cancel, beforeCancel: beforeCancel, info: info}
	r.mu.Lock()
	r.tasks[info.ID] = t
	r.mu.Unlock()
	return ctx, t
}

func (r *TaskRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.tasks, id)
	r.mu.Unlock()
}

// Running lists the running tasks owned by owner (a clone's are its
// source's), oldest first.
func (r *TaskRegistry) Running(owner string) []hook.TaskInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	all := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		all = append(all, t)
	}
	r.mu.Unlock()
	var out []hook.TaskInfo
	for _, t := range all {
		if info := t.Info(); config.SameAgentID(info.Owner, owner) {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b hook.TaskInfo) int { return a.Started.Compare(b.Started) })
	return out
}

// Owns reports whether id is a running task of owner.
func (r *TaskRegistry) Owns(owner, id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	t := r.tasks[id]
	r.mu.Unlock()
	return t != nil && config.SameAgentID(t.Info().Owner, owner)
}

// Cancel cancels the running task id of owner and, first, the tasks it
// started (with theirs), with cause. It returns the tasks it cancelled;
// none when id is not a running task of owner.
func (r *TaskRegistry) Cancel(owner, id string, cause error) []hook.TaskInfo {
	if r == nil || !r.Owns(owner, id) {
		return nil
	}
	return r.stopAll(r.subtree([]string{id}), cause)
}

// CancelAll cancels every running task of owner, with the tasks they
// started, and returns them.
func (r *TaskRegistry) CancelAll(owner string, cause error) []hook.TaskInfo {
	if r == nil {
		return nil
	}
	var roots []string
	for _, info := range r.Running(owner) {
		roots = append(roots, info.ID)
	}
	return r.stopAll(r.subtree(roots), cause)
}

// subtree is roots and every running task started under them, each task
// after the tasks it started.
func (r *TaskRegistry) subtree(roots []string) []*Task {
	r.mu.Lock()
	children := make(map[string][]string, len(r.tasks))
	for id, t := range r.tasks {
		if p := t.Info().Parent; p != "" {
			children[p] = append(children[p], id)
		}
	}
	tasks := maps.Clone(r.tasks)
	r.mu.Unlock()

	var out []*Task
	seen := make(map[string]bool)
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, c := range children[id] {
			visit(c)
		}
		if t := tasks[id]; t != nil {
			out = append(out, t)
		}
	}
	for _, id := range roots {
		visit(id)
	}
	return out
}

// stopAll stops each task in order and returns those this call stopped.
func (r *TaskRegistry) stopAll(list []*Task, cause error) []hook.TaskInfo {
	var out []hook.TaskInfo
	for _, t := range list {
		if info, stopped := t.stop(cause); stopped {
			out = append(out, info)
		}
	}
	return out
}
