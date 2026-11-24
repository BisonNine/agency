// Agency
// License: MIT

package gateway

import (
	"context"
	"time"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools/shell"
)

// shellCloseTimeout bounds the wait for the shell jobs to stop at shutdown:
// each is terminated, then killed after its grace period.
const shellCloseTimeout = 30 * time.Second

// bindShellTools gives the shell tools the loop's hooks once it runs, so the
// notices of jobs lost with the previous process reach their agents.
func bindShellTools(ctx context.Context, loop *agent.AgentLoop) {
	go func() {
		select {
		case <-loop.Started():
		case <-ctx.Done():
			if n := shell.Host.Pending(); n > 0 {
				logger.WarnCF("shell", "Shell job notices not delivered: stopped before the agents started",
					map[string]any{"count": n})
			}
			return
		}
		shell.Host.Bind(loop.ShellHooks())
	}()
}

// closeShellJobs stops every shell job before the process exits. Called
// before the agent loop stops.
func closeShellJobs() {
	ctx, cancel := context.WithTimeout(context.Background(), shellCloseTimeout)
	defer cancel()
	if err := shell.Host.Close(ctx); err != nil {
		logger.WarnCF("shell", "Shell jobs not all stopped at shutdown", map[string]any{"error": err.Error()})
	}
}
