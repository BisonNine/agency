// ClawEh
// License: MIT

package agents

import (
	"errors"

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
	if infos := sm.tasks.Cancel(sm.ownerAgentID, id, errCancelledByAgent); len(infos) > 0 {
		sm.addCancelled(res, infos)
		return res
	}
	rec, err := readStatus(sm.tasksDir(), id)
	if err != nil || !config.SameAgentID(rec.OwnerAgentID, sm.ownerAgentID) {
		res.Unknown = append(res.Unknown, id)
		return res
	}
	if rec.Status == StatusRunning && sm.cancelRecord(rec) {
		res.Cancelled++
		res.Tasks = append(res.Tasks, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: StatusCancelled})
		return res
	}
	res.AlreadyFinished = append(res.AlreadyFinished, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: rec.Status})
	return res
}

// CancelAllTasks cancels every running task of the owner, with the tasks
// they started.
func (sm *SubagentManager) CancelAllTasks() *global.TaskCancelResult {
	res := &global.TaskCancelResult{}
	sm.addCancelled(res, sm.tasks.CancelAll(sm.ownerAgentID, errCancelledByAgent))
	for _, rec := range listStatusRecords(sm.tasksDir()) {
		if rec.Status == StatusRunning && config.SameAgentID(rec.OwnerAgentID, sm.ownerAgentID) && sm.cancelRecord(rec) {
			res.Cancelled++
			res.Tasks = append(res.Tasks, global.TaskBrief{UUID: rec.UUID, Name: rec.Name, Status: StatusCancelled})
		}
	}
	return res
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
// to relaunch): its marker is cleared so it is never relaunched. A task
// running or relaunched meanwhile is left alone.
func (sm *SubagentManager) cancelRecord(rec *TaskRecord) bool {
	if sm.live.Has(rec.UUID) || sm.tasks.Owns(sm.ownerAgentID, rec.UUID) {
		return false
	}
	dir := sm.tasksDir()
	clearRun(dir, rec.UUID)
	rec.Status, rec.Error, rec.FinishedAt = StatusCancelled, cancelledTaskError, nowRFC()
	sm.persistStatus(dir, rec)
	sm.persistResults(dir, cancelledResults(rec, ""))
	logger.InfoCF("subagent", "Task cancelled", map[string]any{
		"uuid": rec.UUID, "name": rec.Name, "owner": sm.ownerAgentID, "kind": taskKind(rec),
		"cause": errCancelledByAgent.Error(),
	})
	return true
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
