// Agency
// License: MIT

package report

import (
	"go.bison9.com/agency/config"
)

// shellJobs is the agent's shell_ctrl state: off without shell_exec, off
// when denied (deny_tools) (a command is then stopped at its timeout),
// else on with the limits its jobs run under.
func shellJobs(cfg *config.Config, a *config.AgentConfig) string {
	switch {
	case !a.IsToolAllowed(config.ShellExecTool):
		return "off"
	case !a.IsToolAllowed(config.ShellCtrlTool):
		return "off (denied); commands stop at their timeout"
	}
	ex := cfg.Tools.Exec.Effective()
	return "on: " + itoa(ex.MaxJobs) + " running at most, each stopped after " + seconds(ex.MaxJobSeconds) +
		", " + byteSize(ex.MaxJobOutputBytes) + " of output each; timeout " + seconds(ex.TimeoutSeconds) +
		" (at most " + seconds(ex.MaxTimeoutSeconds) + ")"
}

// seconds renders a duration in seconds in its largest whole unit.
func seconds(n int) string {
	switch {
	case n > 0 && n%3600 == 0:
		return itoa(n/3600) + " h"
	case n > 0 && n%60 == 0:
		return itoa(n/60) + " min"
	default:
		return itoa(n) + " s"
	}
}

// byteSize renders a size in bytes in its largest whole binary unit.
func byteSize(n int) string {
	switch {
	case n > 0 && n%(1<<20) == 0:
		return itoa(n>>20) + " MiB"
	case n > 0 && n%(1<<10) == 0:
		return itoa(n>>10) + " KiB"
	default:
		return itoa(n) + " bytes"
	}
}
