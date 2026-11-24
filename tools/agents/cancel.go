// Agency
// License: MIT

package agents

import (
	"errors"
	"slices"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
)

// errCancelledByAgent is the cause agent_cancel gives the tasks it stops.
var errCancelledByAgent = &cancelCauseError{text: "cancelled by agent_cancel"}

// cancelCauseError is a cancel cause that is an interrupt (hook.ErrInterrupted).
type cancelCauseError struct{ text string }

func (e *cancelCauseError) Error() string { return e.text }
func (e *cancelCauseError) Unwrap() error { return hook.ErrInterrupted }

// CancelTask cancels the owner's task id and the tasks it started. A task
// that already ended is reported as finished; one that is not the owner's,
// or none at all, as unknown. A task left running by a restart and not yet
// relaunched is settled as cancelled.
func (sm *SubagentManager) CancelTask(id string) *global.TaskCancelResult {
	res := &global.TaskCancelResult{}
	// A task relaunched between the two looks is caught by the next round;
	// each relaunch is bounded by the restart cap.
	for {
		if infos := sm.cancelRunning(id, errCancelledByAgent); len(infos) > 0 {
			sm.addCancelled(res, infos)
			return res
		}
		rec, err := readStatus(sm.tasksDir(), id)
		if err != nil || !config.SameAgentID(rec.OwnerAgentID, sm.ownerAgentID) {
			res.Unknown = append(res.Unknown, id)
			return res
		}
		if rec.Status != StatusRunning {
			res.AlreadyFinished = append(res.AlreadyFinished, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: rec.Status})
			return res
		}
		if sm.hooks.beforeRecordCancel != nil {
			sm.hooks.beforeRecordCancel(id)
		}
		infos, ok := sm.cancelRecord(rec, errCancelledByAgent)
		sm.addCancelled(res, infos)
		if ok {
			res.Cancelled++
			res.Tasks = append(res.Tasks, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: StatusCancelled})
			return res
		}
	}
}

// CancelAllTasks cancels every running task of the owner, with the tasks
// they started (agent_cancel with all).
func (sm *SubagentManager) CancelAllTasks() *global.TaskCancelResult {
	return sm.CancelAllWith(errCancelledByAgent)
}

// CancelAllWith is CancelAllTasks with the cause the tasks are cancelled
// with (/cancel all gives its own).
func (sm *SubagentManager) CancelAllWith(cause error) *global.TaskCancelResult {
	res := &global.TaskCancelResult{}
	sm.addCancelled(res, TasksOf(sm.tasks, sm.ownerAgentID).CancelAll(cause).Cancelled)
	for _, rec := range listStatusRecords(sm.tasksDir()) {
		if rec.Status != StatusRunning || !config.SameAgentID(rec.OwnerAgentID, sm.ownerAgentID) {
			continue
		}
		infos, ok := sm.cancelRecord(rec, cause)
		sm.addCancelled(res, infos)
		if ok {
			res.Cancelled++
			res.Tasks = append(res.Tasks, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: StatusCancelled})
		} else {
			// Relaunched meanwhile: it is in the registry now.
			sm.addCancelled(res, sm.cancelRunning(rec.UUID, cause))
		}
	}
	return res
}

// runs reports whether id is a running task of the owner.
func (sm *SubagentManager) runs(id string) bool {
	return slices.ContainsFunc(TasksOf(sm.tasks, sm.ownerAgentID).Tasks(), func(t hook.TaskInfo) bool { return t.ID == id })
}

// cancelRunning cancels the owner's running task id and, first, the tasks
// it started (with theirs), with cause, and returns the tasks it cancelled;
// none when id is not a running task of the owner (the tasks of one that
// has ended are left alone).
func (sm *SubagentManager) cancelRunning(id string, cause error) []hook.TaskInfo {
	if !sm.runs(id) {
		return nil
	}
	res, err := TasksOf(sm.tasks, sm.ownerAgentID).CancelTask(id, cause)
	if err != nil {
		return nil // hook.ErrUnknownTask: it ended meanwhile and is no longer kept
	}
	return res.Cancelled
}

// addCancelled records the registry's cancelled tasks in res.
func (sm *SubagentManager) addCancelled(res *global.TaskCancelResult, infos []hook.TaskInfo) {
	for _, info := range infos {
		res.Cancelled++
		res.Tasks = append(res.Tasks, global.TaskBrief{UUID: info.ID, Name: info.Label, Status: StatusCancelled})
	}
}

// cancelRecord settles as cancelled a task whose record says running but
// that runs nowhere in this process (a restart left it for the supervisor
// to relaunch), after the tasks it started, which may have been relaunched
// (they are returned): its marker is cleared so it is never relaunched. It
// decides under the process's settle lock, so a relaunch is never
// overtaken: a task running or relaunched meanwhile is left alone (false).
func (sm *SubagentManager) cancelRecord(rec *TaskRecord, cause error) ([]hook.TaskInfo, bool) {
	// Only a task still unfinished takes its children with it.
	if cur, err := readStatus(sm.tasksDir(), rec.UUID); err != nil || cur.Status != StatusRunning {
		return nil, false
	}
	children := sm.tasks.CancelChildren(rec.UUID, cause)
	sm.live.settle.Lock()
	defer sm.live.settle.Unlock()
	if sm.live.Has(rec.UUID) || sm.runs(rec.UUID) {
		return children, false
	}
	// Read again now that nothing runs it: a worker that has just ended
	// wrote its final status before leaving the live set.
	dir := sm.tasksDir()
	cur, err := readStatus(dir, rec.UUID)
	if err != nil || cur.Status != StatusRunning {
		return children, false
	}
	rec = cur
	rec.Status, rec.Error, rec.FinishedAt = StatusCancelled, cancelledTaskError, nowRFC()
	sm.persistStatus(dir, rec)
	sm.persistResults(dir, cancelledResults(rec, ""))
	clearRun(dir, rec.UUID)
	logger.InfoCF("subagent", "Task cancelled", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "owner": sm.ownerAgentID, "kind": taskKind(rec),
		"cause": cause.Error(),
	})
	return children, true
}

// CancelTask implements global.TaskCanceller.
func (s *Spawner) CancelTask(uuid string) (*global.TaskCancelResult, error) {
	if s == nil || s.mgr == nil {
		return nil, errors.New("spawn is not available")
	}
	return s.mgr.CancelTask(uuid), nil
}

// CancelAllTasks implements global.TaskCanceller.
func (s *Spawner) CancelAllTasks() (*global.TaskCancelResult, error) {
	if s == nil || s.mgr == nil {
		return nil, errors.New("spawn is not available")
	}
	return s.mgr.CancelAllTasks(), nil
}
