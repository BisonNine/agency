// Agency
// License: MIT

package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/cron"
	"go.bison9.com/agency/tools"
)

// originCronTool is a cron tool on a store at path whose alice may schedule
// for other agents, and whose callers' origin is origin.
func originCronTool(t *testing.T, path, origin string) *CronTool {
	t.Helper()
	cfg := testConfig()
	for i := range cfg.Agents.List {
		if cfg.Agents.List[i].ID == "alice" {
			cfg.Agents.List[i].GlobalCron = true
		}
	}
	tool := NewCronTool(cron.NewCronService(path, nil), bus.NewMessageBus(), func() *config.Config { return cfg })
	tool.SetOrigin(func(context.Context) string { return origin })
	return tool
}

// firedMessage fires job and returns the message it delivers.
func firedMessage(t *testing.T, tool *CronTool, job *cron.CronJob) bus.InboundMessage {
	t.Helper()
	if out, err := tool.ExecuteJob(context.Background(), job); err != nil || out != "ok" {
		t.Fatalf("ExecuteJob = %q, %v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, ok := tool.msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("nothing delivered")
	}
	return msg
}

// A job keeps the origin of the work that created it, across a restart, and
// each run carries it: a job Alice schedules for herself or for Bob has
// origin alice. A job with none (from the command line, or created before
// origins) carries none, so its runs have the agent they run as.
func TestCronTool_JobOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.json")
	tool := originCronTool(t, path, "alice")
	for _, args := range []map[string]any{
		{"action": "add", "message": "for alice", "at_seconds": float64(600)},
		{"action": "add", "message": "for bob", "at_seconds": float64(600), "agent": "bob"},
	} {
		if res := tool.Execute(agentCtx("alice"), args); res.IsError {
			t.Fatalf("add: %s", res.ForLLM)
		}
	}

	// A new service on the same store is a restart.
	restarted := originCronTool(t, path, "")
	jobs := restarted.cronService.ListJobs(true)
	if len(jobs) != 2 {
		t.Fatalf("%d jobs after the restart, want 2", len(jobs))
	}
	for _, job := range jobs {
		if job.Origin != "alice" {
			t.Errorf("job for %s has origin %q, want alice", job.AgentID, job.Origin)
		}
		msg := firedMessage(t, restarted, &job)
		if got := msg.Metadata[bus.MetaOrigin]; got != "alice" {
			t.Errorf("run of the job for %s carries origin %q, want alice", job.AgentID, got)
		}
	}

	none := jobs[0]
	none.Origin = ""
	if msg := firedMessage(t, restarted, &none); msg.Metadata[bus.MetaOrigin] != "" {
		t.Errorf("a job without an origin carries %q", msg.Metadata[bus.MetaOrigin])
	}
}

// Without a resolver the job takes the inherited origin, else none.
func TestCronTool_CallerOriginWithoutResolver(t *testing.T) {
	tool := newTestCronTool(t)
	if got := tool.callerOrigin(context.Background()); got != "" {
		t.Errorf("nothing inherited = %q", got)
	}
	if got := tool.callerOrigin(tools.WithOrigin(context.Background(), "alice")); got != "alice" {
		t.Errorf("inherited = %q", got)
	}
}

// A watch probe runs the agent's tool with the job's origin.
func TestWatchProbe_JobOrigin(t *testing.T) {
	f := newWatchFixture(t)
	job := f.tool.cronService.ListJobs(true)[0]
	job.Origin = "alice"
	if err := f.tool.cronService.UpdateJob(&job); err != nil {
		t.Fatal(err)
	}
	f.fire()
	if f.probe.sawCtx == nil {
		t.Fatal("probe not called")
	}
	if got := tools.Origin(f.probe.sawCtx); got != "alice" {
		t.Errorf("probe has origin %q, want alice", got)
	}
}
