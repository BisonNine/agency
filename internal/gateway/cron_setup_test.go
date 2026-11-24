// Agency
// License: MIT

package gateway

import (
	"context"
	"testing"
	"time"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/cron"
	"go.bison9.com/agency/global"
)

// TestSetupCronTool_JobsRun: with cron enabled, a due job is executed through
// the cron tool: a plain job is delivered inbound, and a watch job probes the
// owning agent's tools (an unknown agent has none, a missing tool fails), its
// failure counted on the job.
func TestSetupCronTool_JobsRun(t *testing.T) {
	t.Setenv(global.EnvVarHome, t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Agents.BaseDir = t.TempDir()
	cfg.Agents.Defaults.Models = []string{"test-model"}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}}
	cfg.Tools.Cron.Enabled = true
	msgBus := bus.NewMessageBus()
	t.Cleanup(msgBus.Close)
	al, err := agent.NewAgentLoop(t.Context(), cfg, msgBus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close(context.Background()) })

	svc, tool := setupCronTool(al, msgBus, cfg)
	if tool == nil {
		t.Fatal("no cron tool with cron enabled")
	}
	if ierr := svc.Start(); ierr != nil {
		t.Fatal(ierr)
	}
	t.Cleanup(svc.Stop)

	at := time.Now().Add(300 * time.Millisecond).UnixMilli()
	_, err = svc.AddJob("ping", cron.CronSchedule{Kind: "at", AtMS: &at}, "cron says hi", "agent", "cli", "direct", "direct")
	if err != nil {
		t.Fatal(err)
	}
	every := int64(300)
	for _, owner := range []string{"alice", "nobody"} {
		job, err := svc.AddJob("watch-"+owner, cron.CronSchedule{Kind: "every", EveryMS: &every}, "changed", "agent", "cli", "direct", "direct")
		if err != nil {
			t.Fatal(err)
		}
		job.AgentID = owner
		job.Payload.Watch = &cron.CronWatch{Tool: "no_such_tool"}
		if err := svc.UpdateJob(job); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("the due job was not delivered")
	}
	if in.Channel != "cli" {
		t.Errorf("delivered to %q", in.Channel)
	}
	waitFor(t, "both watch probes to fail", func() bool {
		n := 0
		for _, j := range svc.ListJobs(true) {
			if j.Payload.Watch != nil && j.State.WatchFailures > 0 {
				n++
			}
		}
		return n == 2
	})
}
