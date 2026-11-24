// Agency
// License: MIT

package report

import (
	"testing"

	"go.bison9.com/agency/config"
)

// The per-agent "Shell jobs" row names the job tool's state and limits.
func TestCollectAgents_ShellJobsRow(t *testing.T) {
	cfg, env := fixtureConfig(t)
	row := func(id string) string {
		t.Helper()
		_, r := findRow(t, findTable(t, agentSub(t, collectAgents(t.Context(), cfg, env), id), "Settings"), "Shell jobs (shell_ctrl)")
		return r[1]
	}
	cfg.Agents.List[0].Tools = []string{"file_read"}
	cfg.Agents.List[1].Tools = []string{"shell_exec"}
	if got := row("alice"); got != "off" {
		t.Errorf("alice without shell_exec = %q", got)
	}
	if got, want := row("bob"), "on: 8 running at most, each stopped after 24 h, 10 MiB of output each; timeout 1 min (at most 10 min)"; got != want {
		t.Errorf("bob = %q, want %q", got, want)
	}
	cfg.Tools.Exec = config.ExecConfig{TimeoutSeconds: 90, MaxTimeoutSeconds: 3600, MaxJobs: 2, MaxJobSeconds: 7200, MaxJobOutputBytes: 512 << 10}
	if got, want := row("bob"), "on: 2 running at most, each stopped after 2 h, 512 KiB of output each; timeout 90 s (at most 1 h)"; got != want {
		t.Errorf("bob with limits set = %q, want %q", got, want)
	}
	cfg.Agents.List[1].DenyTools = []string{"shell_ctrl"}
	if got := row("bob"); got != "off (denied); commands stop at their timeout" {
		t.Errorf("bob with shell_ctrl denied = %q", got)
	}
}

func TestCollectData_ShellJobLogs(t *testing.T) {
	cfg, env := fixtureConfig(t)
	_, r := findRow(t, findTable(t, collectData(t.Context(), cfg, env), "Storage"), "Shell job logs")
	if r[1] != "<workspace>/state/shell" || r[2] != "output of each shell job; kept 7 days after the job ends; not in backups" {
		t.Errorf("row = %q", r)
	}
	cfg.Tools.Exec.JobLogDays = 2
	if _, r = findRow(t, findTable(t, collectData(t.Context(), cfg, env), "Storage"), "Shell job logs"); r[2] != "output of each shell job; kept 2 days after the job ends; not in backups" {
		t.Errorf("row with job_log_days 2 = %q", r)
	}
}

// shell_ctrl named in tools without shell_exec grants nothing; Check Up says
// so where it is listed.
func TestCollectAgents_ShellCtrlNamedAlone(t *testing.T) {
	cfg, env := fixtureConfig(t)
	cfg.Agents.List[1].Tools = []string{"file_read", "shell_ctrl"}
	m := toolMarkers(findTable(t, agentSub(t, collectAgents(t.Context(), cfg, env), "bob"), "Internal tools"))
	if m["shell_ctrl"] != "ignored (comes with shell_exec)" {
		t.Errorf("shell_ctrl marker = %q", m["shell_ctrl"])
	}
}

// agent_cancel comes with agent_spawn: Check Up lists it with the tool it
// came with, and named alone it grants nothing.
func TestCollectAgents_AgentCancelImplied(t *testing.T) {
	cfg, env := fixtureConfig(t)
	cfg.Agents.List[1].Tools = []string{"file_read", "agent_spawn"}
	m := toolMarkers(findTable(t, agentSub(t, collectAgents(t.Context(), cfg, env), "bob"), "Internal tools"))
	if m["agent_cancel"] != "yes (with agent_spawn)" {
		t.Errorf("agent_cancel marker = %q", m["agent_cancel"])
	}
	cfg.Agents.List[1].Tools = []string{"file_read", "agent_cancel"}
	m = toolMarkers(findTable(t, agentSub(t, collectAgents(t.Context(), cfg, env), "bob"), "Internal tools"))
	if m["agent_cancel"] != "ignored (comes with agent_spawn)" {
		t.Errorf("agent_cancel named alone = %q", m["agent_cancel"])
	}
}
