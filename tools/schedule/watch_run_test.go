package schedule

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/cron"
	"go.bison9.com/agency/tools"
)

// watchFixture is alice's watch on mail_list, whose answer the test sets.
type watchFixture struct {
	t     *testing.T
	tool  *CronTool
	probe *probeTool
	id    string
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	tool := newTestCronTool(t)
	probe := &probeTool{name: "mail_list", result: toolkit.NewResult(`{"ids":[1],"fetched":"10:00"}`)}
	reg := tools.NewToolRegistry()
	reg.Register(probe)
	tool.SetAgentTools(func(agentID string) *tools.ToolRegistry {
		if agentID == "alice" {
			return reg
		}
		return nil
	})
	res := tool.Execute(agentCtx("alice"), map[string]any{
		"action":        "add",
		"message":       "New mail.",
		"every_seconds": float64(300),
		"watch_tool":    "mail_list",
		"watch_fields":  []any{"ids"},
	})
	if res.IsError {
		t.Fatalf("add watch: %s", res.ForLLM)
	}
	jobs := tool.cronService.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	return &watchFixture{t: t, tool: tool, probe: probe, id: jobs[0].ID}
}

// fire runs the job as the scheduler would, with its current state.
func (f *watchFixture) fire() string {
	f.t.Helper()
	for _, j := range f.tool.cronService.ListJobs(true) {
		if j.ID == f.id {
			out, err := f.tool.ExecuteJob(context.Background(), &j)
			if err != nil {
				f.t.Fatalf("ExecuteJob: %v", err)
			}
			return out
		}
	}
	f.t.Fatal("job gone")
	return ""
}

func (f *watchFixture) state() cron.CronJobState {
	f.t.Helper()
	for _, j := range f.tool.cronService.ListJobs(true) {
		if j.ID == f.id {
			return j.State
		}
	}
	f.t.Fatal("job gone")
	return cron.CronJobState{}
}

// delivered is the message the watch put on the bus.
func (f *watchFixture) delivered() bus.InboundMessage {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg, ok := f.tool.msgBus.ConsumeInbound(ctx)
	if !ok {
		f.t.Fatal("nothing delivered")
	}
	return msg
}

// The first probe records a baseline silently; unchanged results stay
// silent; a change in a watched field wakes the agent with the new values,
// on its default channel.
func TestWatch_BaselineSilenceAndChange(t *testing.T) {
	f := newWatchFixture(t)
	if out := f.fire(); out != "no change" || f.state().WatchDigest == "" {
		t.Fatalf("baseline: %q %+v", out, f.state())
	}
	// An unwatched field moves: still silent.
	f.probe.result = toolkit.NewResult(`{"ids":[1],"fetched":"11:00"}`)
	if out := f.fire(); out != "no change" {
		t.Fatalf("unwatched churn: %q", out)
	}
	before := f.state().WatchDigest
	f.probe.result = toolkit.NewResult(`{"ids":[1,2],"fetched":"12:00"}`)
	if out := f.fire(); out != "ok" {
		t.Fatalf("change: %q", out)
	}
	msg := f.delivered()
	if msg.Channel != "telegram-Alice" || msg.ChatID != "chat-alice" || !strings.Contains(msg.Content, "New mail.\n\nWatched fields now read:\nids: [1,2]") {
		t.Errorf("delivered %s/%s: %q", msg.Channel, msg.ChatID, msg.Content)
	}
	if f.state().WatchDigest == before {
		t.Error("digest not advanced after a change")
	}
}

// A failing probe is retried silently, keeps the last digest, and wakes
// the agent once at the threshold; a later success resets the count.
func TestWatch_FailuresReportedOnceAtThreshold(t *testing.T) {
	f := newWatchFixture(t)
	f.fire() // baseline
	baseline := f.state().WatchDigest
	f.probe.result = toolkit.ErrorResult("mailbox locked")
	for i := 1; i < watchFailureNotifyThreshold; i++ {
		if out := f.fire(); out != "no change" || f.state().WatchFailures != i || f.state().WatchDigest != baseline {
			t.Fatalf("failure %d: %q %+v", i, out, f.state())
		}
	}
	if out := f.fire(); out != "ok" {
		t.Fatalf("threshold: %q", out)
	}
	msg := f.delivered()
	if !strings.Contains(msg.Content, `has failed 5 times in a row calling "mail_list" and cannot tell whether anything changed. Last error: tool "mail_list" failed: mailbox locked`) {
		t.Errorf("failure notice: %q", msg.Content)
	}
	// Past the threshold it stays quiet.
	if out := f.fire(); out != "no change" {
		t.Errorf("after threshold: %q", out)
	}
	// The probe recovers with nothing changed: silent, count reset.
	f.probe.result = toolkit.NewResult(`{"ids":[1],"fetched":"13:00"}`)
	if out := f.fire(); out != "no change" || f.state().WatchFailures != 0 {
		t.Errorf("recovery: %q %+v", out, f.state())
	}
}

// A job is skipped, with a reason, when it cannot be delivered.
func TestDeliver_Skips(t *testing.T) {
	ctx := context.Background()
	job := &cron.CronJob{ID: "j1", AgentID: "alice", Payload: cron.CronPayload{Message: "m"}}

	noConfig := NewCronTool(cron.NewCronService(t.TempDir()+"/cron.json", nil), bus.NewMessageBus(), nil)
	if _, err := noConfig.ExecuteJob(ctx, job); err == nil || err.Error() != "configuration not loaded" {
		t.Errorf("no config: %v", err)
	}

	tool := newTestCronTool(t)
	job.AgentID = "nodefault"
	if _, err := tool.ExecuteJob(ctx, job); err == nil || err.Error() != `agent "nodefault" has no default channel; job skipped` {
		t.Errorf("no default channel: %v", err)
	}

	// A closed bus cannot take the message.
	tool.msgBus.Close()
	job.AgentID = "alice"
	if _, err := tool.ExecuteJob(ctx, job); err == nil || !strings.HasPrefix(err.Error(), "queuing cron job: ") {
		t.Errorf("closed bus: %v", err)
	}

	// A listen job is delivered by its listener, not by the schedule.
	listen := &cron.CronJob{ID: "l1", Schedule: cron.CronSchedule{Kind: cron.KindListen}}
	if out, err := tool.ExecuteJob(ctx, listen); err != nil || out != "listen job; delivered by its listener" {
		t.Errorf("listen: %q %v", out, err)
	}
}

func TestSnapshotDescribe(t *testing.T) {
	if got := (watchSnapshot{"": "plain text"}).describe(); got != "plain text" {
		t.Errorf("whole result: %q", got)
	}
	got := (watchSnapshot{"b": []any{1.0}, "a": map[string]any{"x": "y"}, "c": "s"}).describe()
	if got != "a: {\"x\":\"y\"}\nb: [1]\nc: s" {
		t.Errorf("fields: %q", got)
	}
	if got := compactJSON(func() {}); !strings.HasPrefix(got, "0x") {
		t.Errorf("unencodable: %q", got)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := Provider
	if p.Namespace() != "cron" || p.Description() != "Cron scheduling and job management" || p.Category() != "schedule" || p.ConfigKey() != "cron" {
		t.Errorf("metadata: %q %q %q %q", p.Namespace(), p.Description(), p.Category(), p.ConfigKey())
	}
	if ok, why := p.Available(&config.Config{}); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	if d := p.Describe(); len(d) != 1 || d[0].Name != "cron_schedule" || !d[0].DefaultEnabled {
		t.Errorf("Describe = %+v", d)
	}
	if p.Build(tools.ToolDeps{}) != nil {
		t.Error("the cron tool is built by the gateway")
	}
	if !(&CronTool{}).IsSessionScoped() {
		t.Error("cron_schedule is session-scoped")
	}
}

// A watch whose job was removed meanwhile still decides from the copy it
// fired with; failing to save its state is logged, never fatal.
func TestWatch_StateNotSaved(t *testing.T) {
	f := newWatchFixture(t)
	var job cron.CronJob
	for _, j := range f.tool.cronService.ListJobs(true) {
		job = j
	}
	if _, err := f.tool.cronService.RemoveJob(job.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if out, err := f.tool.ExecuteJob(ctx, &job); err != nil || out != "no change" {
		t.Fatalf("baseline: %q %v", out, err)
	}
	job.State.WatchDigest = "stale"
	if out, err := f.tool.ExecuteJob(ctx, &job); err != nil || out != "ok" {
		t.Fatalf("change: %q %v", out, err)
	}
	f.delivered()
	f.probe.result = toolkit.ErrorResult("down")
	if out, err := f.tool.ExecuteJob(ctx, &job); err != nil || out != "no change" {
		t.Fatalf("failure: %q %v", out, err)
	}
	f.tool.persistListenState(&job, "d", 1) // logged only
}

func TestListenEvent_EmptyAndMissingField(t *testing.T) {
	if _, ok := listenEvent("", nil); ok {
		t.Error("an empty result is no event")
	}
	if _, ok := listenEvent(`{"other":1}`, []string{"id"}); ok {
		t.Error("a result without the watched field is no event")
	}
	if d, ok := listenEvent(`{"id":7}`, []string{"id"}); !ok || d == "" {
		t.Error("a result with the field is an event")
	}
}

// A watch on the whole result says so when it is created.
func TestWatch_AddWithoutFields(t *testing.T) {
	tool := newTestCronTool(t)
	reg := tools.NewToolRegistry()
	reg.Register(&probeTool{name: "mail_list", result: toolkit.NewResult("x")})
	tool.SetAgentTools(func(string) *tools.ToolRegistry { return reg })
	res := tool.Execute(agentCtx("alice"), map[string]any{"action": "add", "message": "m", "every_seconds": float64(300), "watch_tool": "mail_list"})
	if res.IsError || !strings.Contains(res.ForLLM, "the tool's result") {
		t.Errorf("add: %s", res.ForLLM)
	}
}

// A probe cut short because the scheduler stopped is not a failed probe: the
// failure count and the remembered state stay as they were.
func TestWatch_ProbeStoppedByScheduler(t *testing.T) {
	f := newWatchFixture(t)
	if out := f.fire(); out != "no change" {
		t.Fatalf("baseline: %q", out)
	}
	before := f.state()
	f.probe.result = toolkit.ErrorResult("interrupted")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var job cron.CronJob
	for _, j := range f.tool.cronService.ListJobs(true) {
		job = j
	}
	if out, err := f.tool.ExecuteJob(ctx, &job); err != nil || out != "no change" {
		t.Fatalf("stopped probe: %q %v", out, err)
	}
	if after := f.state(); after.WatchFailures != 0 || after.WatchDigest != before.WatchDigest {
		t.Errorf("state changed by a stopped probe: %+v, was %+v", after, before)
	}

	// The same failure while the scheduler runs is counted.
	if out := f.fire(); out != "no change" || f.state().WatchFailures != 1 {
		t.Errorf("failed probe: %q %+v", out, f.state())
	}
}

// A job due as the scheduler stops still queues its message.
func TestExecuteJob_DeliversWhenSchedulerStopped(t *testing.T) {
	tool := newTestCronTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := &cron.CronJob{ID: "j1", AgentID: "alice", Payload: cron.CronPayload{Message: "report"}}
	if out, err := tool.ExecuteJob(ctx, job); err != nil || out != "ok" {
		t.Fatalf("ExecuteJob: %q %v", out, err)
	}
	recv, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if msg, ok := tool.msgBus.ConsumeInbound(recv); !ok || !strings.Contains(msg.Content, "report") {
		t.Errorf("not delivered: %+v %v", msg, ok)
	}
}
