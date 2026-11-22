// ClawEh
// License: MIT

package agent

import (
	toolsshell "go.bison9.com/agency/tools/shell"
)

// backgroundRunning counts what owner (an agent id; a clone's work is its
// source's) has running in the background: its sub-agent and task-model
// tasks and its shell jobs.
func (al *AgentLoop) backgroundRunning(owner string) int {
	return len(al.tasks.Running(owner)) + toolsshell.Host.Running(owner)
}

// cancelBackground cancels everything owner runs in the background (/cancel
// all): every task, cascading to the tasks they started, and every shell job.
// It returns how many it stopped.
func (al *AgentLoop) cancelBackground(owner string) int {
	return len(al.tasks.CancelAll(owner, errCancelledByUser)) + toolsshell.Host.StopAll(owner)
}
