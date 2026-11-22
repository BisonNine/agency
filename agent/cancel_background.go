// ClawEh
// License: MIT

package agent

import (
	toolsagents "go.bison9.com/agency/tools/agents"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// backgroundRunning counts what owner (an agent id; a clone's work is its
// source's) has running in the background: its sub-agent and task-model
// tasks and its shell jobs.
func (al *AgentLoop) backgroundRunning(owner string) int {
	return len(al.tasks.Running(owner)) + toolsshell.Host.Running(owner)
}

// cancelBackground cancels everything agent runs in the background (/cancel
// all): every task, cascading to the tasks they started (the path of
// agent_cancel with all), and every shell job. It returns how many it
// stopped.
func (al *AgentLoop) cancelBackground(agent *AgentInstance) int {
	owner := agent.toolIdentity()
	n := toolsshell.Host.StopAll(owner)
	if mgr := al.spawnManagerOf(owner); mgr != nil {
		return n + mgr.CancelAllWith(errCancelledByUser).Cancelled
	}
	return n + len(al.tasks.CancelAll(owner, errCancelledByUser))
}

// spawnManagerOf is the sub-agent manager of the config agent id, which
// also holds its clones' tasks; nil when it has none.
func (al *AgentLoop) spawnManagerOf(id string) *toolsagents.SubagentManager {
	if a, ok := al.GetRegistry().Get(id); ok && a.spawnMgr != nil {
		return a.spawnMgr
	}
	return nil
}
