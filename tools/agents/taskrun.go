// ClawEh
// License: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// taskRun is one task of the manager while it runs: its record, its entry in
// the registry and how its result is delivered.
type taskRun struct {
	rec  *TaskRecord
	task *Task

	mu sync.Mutex
	// cb receives the result of a callback task (and of a wait task its turn
	// stopped waiting for).
	cb tools.AsyncCallback
	// result receives the result of a wait task (buffered, set at launch,
	// never changed) while waiting says a turn still waits for it.
	result  chan *tools.ToolResult
	waiting bool
	// ended: the result was recorded, or the task was cancelled.
	ended     bool
	cancelled bool
	// settleHeld: the task is being started by a caller holding the
	// settle lock (launchTask).
	settleHeld bool
}

// cancelledTaskError is the error a cancelled task's record carries.
const cancelledTaskError = "cancelled"

// taskMode maps a record's mode to the registry's.
func taskMode(mode string) hook.TaskMode {
	if mode == "wait" {
		return hook.TaskWait
	}
	return hook.TaskCallback
}

// taskKind is what the registry calls a record's task.
func taskKind(rec *TaskRecord) string {
	if rec.Kind != "" {
		return rec.Kind
	}
	return "subagent"
}

// launch registers rec's task under its parent task and runs it on a
// context of its own. A wait-mode task (wait) delivers its result to the
// returned run's waiter; any other to cb. askChain is the ask chain of the
// turn that started it.
func (sm *SubagentManager) launch(parent context.Context, rec *TaskRecord, cb tools.AsyncCallback, resumed, wait bool, askChain []string) *taskRun {
	return sm.launchTask(parent, rec, cb, resumed, wait, askChain, false)
}

// launchTask is launch; settleHeld says the caller holds the settle lock
// (the supervisor), which a task cancelled as it starts (its parent was
// cancelled) must then not take again.
func (sm *SubagentManager) launchTask(parent context.Context, rec *TaskRecord, cb tools.AsyncCallback, resumed, wait bool, askChain []string, settleHeld bool) *taskRun {
	tr := &taskRun{rec: rec, cb: cb, settleHeld: settleHeld}
	if wait {
		tr.result, tr.waiting = make(chan *tools.ToolResult, 1), true
	}
	info := hook.TaskInfo{
		ID: rec.UUID, Owner: sm.ownerAgentID, Parent: rec.Parent,
		Kind: taskKind(rec), Label: rec.Name, Mode: taskMode(rec.Mode),
	}
	ctx, task := sm.tasks.start(parent, info, rec.SpawnDepth, askChain, func(cause error) bool { return sm.cancelRun(tr, cause) })
	tr.mu.Lock()
	tr.task, tr.settleHeld = task, false
	tr.mu.Unlock()
	go sm.runRecord(ctx, tr, resumed)
	return tr
}

// runRecord executes a task to completion on its own context and records
// it (complete). resumed prepends the interruption note. Panics are
// recovered and recorded as errors.
func (sm *SubagentManager) runRecord(ctx context.Context, tr *taskRun, resumed bool) {
	rec := tr.rec
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)
			if rec.Kind != "" {
				sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordTaskResults(rec, nil, err) }, "", nil)
				return
			}
			sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordResults(rec, "", 0, err) }, "", nil)
		}
	}()

	tr.mu.Lock()
	rec.StartedAt = nowRFC()
	if !tr.waiting {
		sm.persistStatus(sm.tasksDir(), rec)
	}
	tr.mu.Unlock()

	if rec.Kind != "" {
		res, err := sm.runTaskModel(ctx, rec, string(tr.task.Info().Mode))
		sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordTaskResults(rec, res, err) }, "", nil)
		return
	}

	taskText := rec.Task
	if resumed {
		taskText = interruptionNote + "\n\n" + rec.Task
	}
	if sm.runFull == nil {
		sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordResults(rec, "", 0, errRunnerNotConfigured()) }, "", nil)
		return
	}

	// Run a temporary clone of the agent; it is deleted once the result has
	// been delivered.
	target := sm.targetAgent(rec.AgentID)
	fr, release, err := sm.runFull(ctx, target, taskText, rec.Model, rec.CharacterCard, rec.Media)
	if err != nil {
		partial := ""
		if fr != nil {
			partial = fr.Content
		}
		logger.WarnCF("subagent", "subagent.run.failed", map[string]any{
			"uuid": rec.UUID, "name": rec.Name, "agent": target, "mode": tr.task.Info().Mode, "error": err.Error(),
		})
		sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordResults(rec, "", 0, err) }, partial, release)
		return
	}
	logger.InfoCF("subagent", "subagent.run.finished", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "agent": target, "mode": tr.task.Info().Mode,
		"iterations": fr.Iterations, "content_len": len(fr.Content),
	})
	sm.complete(ctx, tr, func() *tools.ToolResult { return sm.recordResults(rec, fr.Content, fr.Iterations, nil) }, fr.Content, release)
}

// complete records a task that ended: record builds its results and status
// files and returns the notification, which goes to the waiting turn or to
// the callback (release, when not nil, deletes the worker's clone: before a
// waiting turn gets the result, after a callback). A cancelled task is not recorded again: only its partial
// output, when there is some, is kept in its results file.
func (sm *SubagentManager) complete(ctx context.Context, tr *taskRun, record func() *tools.ToolResult, partial string, release func()) {
	if release == nil {
		release = func() {}
	}
	tr.mu.Lock()
	if tr.ended {
		cancelled := tr.cancelled
		tr.mu.Unlock()
		if cancelled && partial != "" {
			sm.persistResults(sm.tasksDir(), cancelledResults(tr.rec, partial))
		}
		release()
		tr.task.finished(hook.TaskCancelled)
		// The cancel told the observers already; they hear once more that
		// the worker has unwound.
		sm.tasks.notify(tr.task.Info())
		return
	}
	tr.ended = true
	result := record()
	dir := sm.tasksDir()
	clearRun(dir, tr.rec.UUID)
	sm.live.Remove(tr.rec.UUID)
	waiting, cb := tr.waiting, tr.cb
	tr.waiting = false
	status := tr.rec.Status
	tr.mu.Unlock()
	state := hook.TaskDone
	if status != StatusDone {
		state = hook.TaskError
	}
	tr.task.finished(state)

	// A waiting turn gets its result once the clone is gone; a callback is
	// delivered first, then the clone is released.
	if waiting {
		release()
		tr.result <- result
		return
	}
	defer release()
	if cb != nil {
		logger.InfoCF("subagent", "subagent.callback.fired", map[string]any{
			"uuid": tr.rec.UUID, "name": tr.rec.Name, "agent": sm.targetAgent(tr.rec.AgentID),
			"status": status, "result_file": tr.rec.ResultsPath,
		})
		cb(context.WithoutCancel(ctx), result)
	}
}

// cancelledResults is the results file of a cancelled task: what it had
// produced, marked incomplete.
func cancelledResults(rec *TaskRecord, content string) *TaskResults {
	return &TaskResults{
		UUID: rec.UUID, Name: rec.Name, Status: StatusCancelled,
		FinishedAt: rec.FinishedAt, Content: content, Incomplete: true,
	}
}

// cancelRun settles a task being cancelled, before its context is: the
// record says cancelled, the run marker is cleared and the task leaves the
// live set, so neither the supervisor nor a restart relaunches it. No
// callback fires; a turn still waiting for it is told it was cancelled. It
// reports false for a task that had already ended.
func (sm *SubagentManager) cancelRun(tr *taskRun, cause error) bool {
	tr.mu.Lock()
	if tr.ended {
		tr.mu.Unlock()
		return false
	}
	tr.ended, tr.cancelled = true, true
	rec := tr.rec
	rec.Status, rec.Error, rec.FinishedAt = StatusCancelled, cancelledTaskError, nowRFC()
	dir := sm.tasksDir()
	// The record says cancelled before the marker goes and the task leaves
	// the live set, all under the settle lock: the supervisor never sees it
	// unmarked-live yet running.
	if !tr.settleHeld {
		sm.live.settle.Lock()
	}
	sm.persistStatus(dir, rec)
	sm.persistResults(dir, cancelledResults(rec, ""))
	if sm.hooks.settling != nil {
		sm.hooks.settling(rec.UUID)
	}
	clearRun(dir, rec.UUID)
	sm.live.Remove(rec.UUID)
	if !tr.settleHeld {
		sm.live.settle.Unlock()
	}
	waiting := tr.waiting
	tr.waiting = false
	tr.mu.Unlock()
	if sm.hooks.settled != nil {
		sm.hooks.settled(rec.UUID)
	}

	logger.InfoCF("subagent", "Task cancelled", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "owner": sm.ownerAgentID, "kind": taskKind(rec),
		"cause": fmt.Sprint(cause),
	})
	if waiting {
		tr.result <- &tools.ToolResult{
			ForLLM:  fmt.Sprintf("Task '%s' (uuid %s) was cancelled before it finished.", rec.Name, rec.UUID),
			IsError: true,
		}
	}
	return true
}

// detach turns a wait task whose turn stopped waiting into a callback task:
// it goes on, its record says callback and gets a run marker (so a restart
// relaunches it), and its result goes to cb. It returns false when the task
// had already ended; its result is then in tr.result.
func (sm *SubagentManager) detach(tr *taskRun, cb tools.AsyncCallback) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.ended || !tr.waiting {
		return false
	}
	tr.waiting, tr.cb = false, cb
	rec := tr.rec
	rec.Mode = "callback"
	rec.RetryAfter = nowEpoch() + retryDelaySecs()
	sm.live.Add(rec.UUID)
	dir := sm.tasksDir()
	sm.persistStatus(dir, rec)
	if err := markRun(dir, rec.UUID); err != nil {
		logger.WarnCF("subagent", "failed to write task marker", map[string]any{"uuid": rec.UUID, "error": err.Error()})
	}
	tr.task.setMode(hook.TaskCallback)
	logger.InfoCF("subagent", "Task detached: its turn stopped waiting", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "owner": sm.ownerAgentID, "kind": taskKind(rec),
	})
	return true
}

// await waits for a wait task's result. When ctx ends first the task is not
// stopped: it is detached and its result is delivered later to onDetach. It
// is cancelled instead when the process is shutting down (the turn is
// replayed on restart and starts it again), and when there is no onDetach
// (no chat for a later result).
func (sm *SubagentManager) await(ctx context.Context, tr *taskRun, onDetach tools.AsyncCallback) *tools.ToolResult {
	select {
	case res := <-tr.result:
		return res
	case <-ctx.Done():
	}
	cause := context.Cause(ctx)
	if sm.shuttingDown() {
		cause = hook.ErrShutdown
	}
	if isShutdown(cause) {
		tr.task.stop(cause)
		return <-tr.result
	}
	if onDetach == nil {
		// Nowhere to deliver a later result (no chat to answer in, as for a
		// callback spawn, which is refused there): the task is cancelled
		// rather than left to report to no one.
		tr.task.stop(errNoRoute)
		return <-tr.result
	}
	if !sm.detach(tr, onDetach) {
		return <-tr.result
	}
	return &tools.ToolResult{ForLLM: fmt.Sprintf(detachedText, tr.rec.Name, tr.rec.UUID, tr.rec.UUID)}
}

// errNoRoute cancels a wait task whose turn stopped waiting when its result
// would have nowhere to go.
var errNoRoute = &cancelCauseError{text: "its turn stopped waiting and its result has nowhere to go"}

// detachedText is the tool result of a wait spawn whose turn stopped
// waiting (an interrupt, or the turn's time ran out).
const detachedText = "Stopped waiting for task '%s' (uuid %s): it goes on in the background and its result " +
	"will arrive later as a background result, or poll agent_status with uuid=%q."

// isShutdown reports a cancel cause that is the host shutting down.
func isShutdown(cause error) bool {
	return cause != nil && errors.Is(cause, hook.ErrShutdown)
}
