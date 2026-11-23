// ClawEh
// License: MIT

package agents

import (
	"slices"
	"strings"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"

	"go.bison9.com/agency/config"
)

// TaskOwner is the one spelling of an agent id the process's task registry
// (loop.Tasks) is given as an owner: the registry compares owners exactly,
// while "Alice" and "alice" are one agent (config.SameAgentID).
func TaskOwner(agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return agentID
	}
	return config.NormalizeAgentID(agentID)
}

// TasksOf is the process's task registry as the agent agentID sees it (a
// clone's tasks are its source's): it starts tasks owned by that agent, and
// lists and cancels only those.
func TasksOf(tasks *loop.Tasks, agentID string) hook.TaskRegistrar {
	return tasks.For(TaskOwner(agentID))
}

// RunningTasks lists the running tasks of the agent agentID, oldest first.
func RunningTasks(tasks *loop.Tasks, agentID string) []hook.TaskInfo {
	list := TasksOf(tasks, agentID).Tasks()
	slices.SortStableFunc(list, func(a, b hook.TaskInfo) int { return a.Started.Compare(b.Started) })
	return list
}
