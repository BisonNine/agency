// Agency
// License: MIT

package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// taskRun is one task of the manager while it runs: its record, its handle
// in the process's task registry and how its result is delivered.
type taskRun struct {
	rec *TaskRecord
	// h is the task's handle; nil when the registry refused to start it.
	h hook.TaskHandle

	mu sync.Mutex
	// cb receives the result of a callback task (and of a wait task its turn
	// stopped waiting for).
	cb toolkit.AsyncCallback
	// result receives the result of a wait task (buffered, set at launch,
	// never changed) while waiting says a turn still waits for it.
	result  chan *toolkit.Result
	waiting bool
	// ended: the result was recorded, or the task was cancelled.
	ended     bool
	cancelled bool
	// settleHeld: the task is being started by a caller holding the
	// settle lock (launchTask).
	settleHeld bool
	// detachTo receives the result once the turn waiting for the task
	// stopped waiting (await, detach).
	detachTo toolkit.AsyncCallback
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
func (sm *SubagentManager) launch(parent context.Context, rec *TaskRecord, cb toolkit.AsyncCallback, resumed, wait bool, askChain []string) *taskRun {
	return sm.launchTask(parent, rec, cb, resumed, wait, askChain, false)
}

// launchTask is launch; settleHeld says the caller holds the settle lock
// (the supervisor), which a task cancelled as it starts (its parent was
// cancelled) must then not take again.
func (sm *SubagentManager) launchTask(parent context.Context, rec *TaskRecord, cb toolkit.AsyncCallback, resumed, wait bool, askChain []string, settleHeld bool) *taskRun {
	tr := &taskRun{rec: rec, cb: cb, settleHeld: settleHeld}
	if wait {
		tr.result, tr.waiting = make(chan *toolkit.Result, 1), true
	}
	ctx, h, err := TasksOf(sm.tasks, sm.ownerAgentID).StartTask(parent, hook.TaskStart{
		Info: hook.TaskInfo{
			ID: rec.UUID, Parent: rec.Parent,
			Kind: taskKind(rec), Label: rec.Name, Mode: taskMode(rec.Mode),
		},
		Depth:    rec.SpawnDepth,
		OnCancel: func(cause error) bool { return sm.cancelRun(tr, cause) },
		OnDetach: func() bool { return sm.detach(tr) },
	})
	tr.mu.Lock()
	tr.h, tr.settleHeld = h, false
	tr.mu.Unlock()
	if err != nil {
		sm.startFailed(parent, tr, err)
		return tr
	}
	// The task's context carries nothing of the turn's but its scope; the
	// host's own values go on it here.
	ctx = WithSpawnDepth(ctx, rec.SpawnDepth)
	if len(askChain) > 0 {
		ctx = tools.WithAskChain(ctx, askChain)
	}
	ctx = sm.withTaskBilling(ctx, tr)
	go sm.runRecord(ctx, tr, resumed)
	return tr
}

// withTaskBilling puts the task's billing code on its context, as the code
// its work inherits (a record written before billing codes runs under the
// default). A change the task makes (its billing tool) is written to its
// record, so a relaunch after a restart keeps it.
func (sm *SubagentManager) withTaskBilling(ctx context.Context, tr *taskRun) context.Context {
	tr.mu.Lock()
	code := tools.CheckedBilling(tr.rec.Billing, "task "+tr.rec.UUID)
	tr.mu.Unlock()
	ctx = tools.WithBilling(ctx, code)
	tools.InheritedBillingOf(ctx).OnSet(func(code string) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.rec.Billing = code
		// A waited task's record is written when it ends or is detached;
		// an ended one's is final.
		if !tr.waiting && !tr.ended {
			sm.persistStatus(sm.tasksDir(), tr.rec)
		}
	})
	return ctx
}

// startFailed settles a task the registry refused to start. While the
// process shuts down, a waited task is cancelled (its turn is replayed and
// starts it again) and a background one keeps its record and marker, so a
// restart relaunches it. Any other refusal (a duplicate id) is the task's
// error, delivered on ctx (the starter's, without its cancellation).
func (sm *SubagentManager) startFailed(ctx context.Context, tr *taskRun, err error) {
	logger.WarnCF("subagent", "Task not started", map[string]any{
		"uuid": tr.rec.UUID, "name": tr.rec.Name, "owner": sm.ownerAgentID, "kind": taskKind(tr.rec), "error": err.Error(),
	})
	if isShutdown(err) {
		if tr.waiting {
			sm.cancelRun(tr, err)
		}
		return
	}
	rec, ctx := tr.rec, context.WithoutCancel(ctx)
	if rec.Kind != "" {
		sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordTaskResults(rec, nil, err) }, "", nil)
		return
	}
	sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordResults(rec, "", 0, err) }, "", nil)
}

// runRecord executes a task to completion on its own context and records
// it (complete). resumed prepends the interruption note. Panics are
// recovered and recorded as errors.
func (sm *SubagentManager) runRecord(ctx context.Context, tr *taskRun, resumed bool) {
	rec := tr.rec
	tr.mu.Lock()
	h := tr.h
	tr.mu.Unlock()
	// Last: the worker is gone and has written all it writes.
	defer h.Exited()
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)
			if rec.Kind != "" {
				sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordTaskResults(rec, nil, err) }, "", nil)
				return
			}
			sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordResults(rec, "", 0, err) }, "", nil)
		}
	}()

	tr.mu.Lock()
	if tr.ended {
		// Cancelled before it started: its record is final, nothing runs.
		tr.mu.Unlock()
		return
	}
	rec.StartedAt = nowRFC()
	if !tr.waiting {
		sm.persistStatus(sm.tasksDir(), rec)
	}
	tr.mu.Unlock()

	if rec.Kind != "" {
		res, err := sm.runTaskModel(ctx, rec, string(h.Info().Mode))
		sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordTaskResults(rec, res, err) }, "", nil)
		return
	}

	taskText := rec.Task
	if resumed {
		taskText = interruptionNote + "\n\n" + rec.Task
	}
	if sm.runFull == nil {
		sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordResults(rec, "", 0, errRunnerNotConfigured()) }, "", nil)
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
			"uuid": rec.UUID, "name": rec.Name, "agent": target, "mode": h.Info().Mode, "error": err.Error(),
		})
		sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordResults(rec, "", 0, err) }, partial, release)
		return
	}
	logger.InfoCF("subagent", "subagent.run.finished", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "agent": target, "mode": h.Info().Mode,
		"iterations": fr.Iterations, "content_len": len(fr.Content),
	})
	sm.complete(ctx, tr, func() *toolkit.Result { return sm.recordResults(rec, fr.Content, fr.Iterations, nil) }, fr.Content, release)
}

// complete records a task that ended: record builds its results and status
// files and returns the notification, which goes to the waiting turn or to
// the callback (release, when not nil, deletes the worker's clone: before a
// waiting turn gets the result, after a callback). A cancelled task is not recorded again: only its partial
// output, when there is some, is kept in its results file.
func (sm *SubagentManager) complete(ctx context.Context, tr *taskRun, record func() *toolkit.Result, partial string, release func()) {
	if release == nil {
		release = func() {}
	}
	tr.mu.Lock()
	if tr.ended {
		cancelled := tr.cancelled
		tr.mu.Unlock()
		// The partial output, unless the cancel stopped waiting for it:
		// the record it wrote is then final. The observers hear once more
		// that the worker has unwound when it reports it exited.
		if cancelled && partial != "" && !tr.h.Sealed() {
			sm.persistResults(sm.tasksDir(), cancelledResults(tr.rec, partial))
		}
		release()
		return
	}
	if isShutdown(context.Cause(ctx)) {
		// The process is shutting down (Tasks.Shutdown): the task is not
		// over, its record stays as it is and a restart relaunches it.
		tr.mu.Unlock()
		release()
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
	if tr.h != nil {
		tr.h.Finish(result)
	}

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
		tr.result <- &toolkit.Result{
			ForLLM:  fmt.Sprintf("Task '%s' (uuid %s) was cancelled before it finished.", rec.Name, rec.UUID),
			IsError: true,
		}
	}
	return true
}

// detach is the record half of detaching a wait task whose turn stopped
// waiting (the task's OnDetach): it goes on as a callback task, its record
// says callback and gets a run marker (so a restart relaunches it), and its
// result goes to tr.detachTo. It returns false when the task had already
// ended; its result is then in tr.result.
func (sm *SubagentManager) detach(tr *taskRun) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.ended || !tr.waiting {
		return false
	}
	tr.waiting, tr.cb = false, tr.detachTo
	rec := tr.rec
	rec.Mode = "callback"
	rec.RetryAfter = nowEpoch() + retryDelaySecs()
	sm.live.Add(rec.UUID)
	dir := sm.tasksDir()
	sm.persistStatus(dir, rec)
	if err := markRun(dir, rec.UUID); err != nil {
		logger.WarnCF("subagent", "failed to write task marker", map[string]any{"uuid": rec.UUID, "error": err.Error()})
	}
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
func (sm *SubagentManager) await(ctx context.Context, tr *taskRun, onDetach toolkit.AsyncCallback) *toolkit.Result {
	if tr.h == nil {
		// Never started: startFailed has delivered its result.
		return <-tr.result
	}
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
		tr.h.Cancel(cause)
		return <-tr.result
	}
	if onDetach == nil {
		// Nowhere to deliver a later result (no chat to answer in, as for a
		// callback spawn, which is refused there): the task is cancelled
		// rather than left to report to no one.
		tr.h.Cancel(errNoRoute)
		return <-tr.result
	}
	tr.mu.Lock()
	tr.detachTo = onDetach
	tr.mu.Unlock()
	if !tr.h.Detach(cause) {
		return <-tr.result
	}
	return &toolkit.Result{ForLLM: fmt.Sprintf(detachedText, tr.rec.Name, tr.rec.UUID, tr.rec.UUID)}
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
