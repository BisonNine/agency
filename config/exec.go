// ClawEh
// License: MIT

package config

import "errors"

// The defaults of tools.exec's timeouts and job limits.
const (
	DefaultExecTimeoutSeconds    = 60
	DefaultExecMaxTimeoutSeconds = 600
	DefaultExecMaxJobs           = 8
	DefaultExecMaxJobSeconds     = 86400
	DefaultExecMaxJobOutputBytes = 10 << 20
	DefaultExecJobLogDays        = 7
)

// ShellJobsDir is where the shell tools keep each job's log and record,
// relative to the agent's workspace. The file tools may read it, never
// write it (AlwaysReadableWorkspaceDirs), and the backup leaves it out.
const ShellJobsDir = "state/shell"

// Effective returns the shell settings with every unset (0 or less) timeout
// and limit at its default. A default timeout above the maximum is the
// maximum.
func (e ExecConfig) Effective() ExecConfig {
	orDefault := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	out := e
	out.MaxTimeoutSeconds = orDefault(e.MaxTimeoutSeconds, DefaultExecMaxTimeoutSeconds)
	out.TimeoutSeconds = min(orDefault(e.TimeoutSeconds, DefaultExecTimeoutSeconds), out.MaxTimeoutSeconds)
	out.MaxJobs = orDefault(e.MaxJobs, DefaultExecMaxJobs)
	out.MaxJobSeconds = orDefault(e.MaxJobSeconds, DefaultExecMaxJobSeconds)
	out.MaxJobOutputBytes = orDefault(e.MaxJobOutputBytes, DefaultExecMaxJobOutputBytes)
	out.JobLogDays = orDefault(e.JobLogDays, DefaultExecJobLogDays)
	return out
}

// Validate refuses a negative timeout or limit, naming every one.
func (e ExecConfig) Validate() error {
	var errs []error
	for _, f := range []struct {
		name  string
		value int
	}{
		{"timeout_seconds", e.TimeoutSeconds},
		{"max_timeout_seconds", e.MaxTimeoutSeconds},
		{"max_jobs", e.MaxJobs},
		{"max_job_seconds", e.MaxJobSeconds},
		{"max_job_output_bytes", e.MaxJobOutputBytes},
		{"job_log_days", e.JobLogDays},
	} {
		if f.value < 0 {
			errs = append(errs, errors.New("The shell setting tools.exec."+f.name+" must be 0 (the default) or more."))
		}
	}
	return errors.Join(errs...)
}
