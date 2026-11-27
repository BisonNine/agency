package schedule

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/cron"
)

// With no calling agent in the context nothing is done: cron fails closed.
func TestCronTool_NoCaller(t *testing.T) {
	tool := newTestCronTool(t)
	for _, action := range []string{"add", "list", "get", "remove"} {
		res := tool.Execute(context.Background(), map[string]any{"action": action, "message": "m", "at_seconds": float64(60), "job_id": "x"})
		if !res.IsError || res.ForLLM != "no calling agent in context" {
			t.Errorf("%s: %+v", action, res)
		}
	}
	if len(tool.cronService.ListJobs(true)) != 0 {
		t.Error("a job was created without a caller")
	}
}

func TestCronTool_AddRefusals(t *testing.T) {
	noConfig := NewCronTool(cron.NewCronService(t.TempDir()+"/cron.json", nil), bus.NewMessageBus(), func() *config.Config { return nil })
	if res := noConfig.Execute(agentCtx("alice"), map[string]any{"action": "add", "message": "m", "at_seconds": float64(60)}); !res.IsError || res.ForLLM != "cron unavailable: configuration not loaded" {
		t.Errorf("no config: %+v", res)
	}
	tool := newTestCronTool(t)
	res := tool.Execute(agentCtx("alice"), map[string]any{"action": "add", "message": "m", "at_seconds": float64(60), "watch_fields": []any{"x"}})
	if !res.IsError || res.ForLLM != "watch_fields was given without watch_tool" {
		t.Errorf("half a watch: %+v", res)
	}
	if len(tool.cronService.ListJobs(true)) != 0 {
		t.Error("a refused add created a job")
	}
}

// list and get show every kind of schedule, the state and the runs; a
// disabled job stays listed, so the agent can find it to enable it again.
func TestCronTool_ListAndGetFormats(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := agentCtx("alice")
	for _, args := range []map[string]any{
		{"action": "add", "message": "daily", "cron_expr": "0 9 * * *"},
		{"action": "add", "message": "once", "at_seconds": float64(600)},
	} {
		if res := tool.Execute(ctx, args); res.IsError {
			t.Fatalf("add: %s", res.ForLLM)
		}
	}
	var daily string
	for _, j := range tool.cronService.ListJobs(true) {
		if j.Payload.Message == "daily" {
			daily = j.ID
		}
	}
	if res := tool.Execute(ctx, map[string]any{"action": "disable", "job_id": daily}); res.IsError || res.ForLLM != "Cron job 'daily' disabled" {
		t.Fatalf("disable: %+v", res)
	}
	list := tool.Execute(ctx, map[string]any{"action": "list"}).ForLLM
	if !strings.Contains(list, "0 9 * * *, disabled, next: —") || !strings.Contains(list, "one-time, enabled, next: ") {
		t.Errorf("list:\n%s", list)
	}
	get := tool.Execute(ctx, map[string]any{"action": "get", "job_id": daily}).ForLLM
	if !strings.Contains(get, "schedule: 0 9 * * *") || !strings.Contains(get, "status:   disabled") || !strings.Contains(get, "next run: —") {
		t.Errorf("get:\n%s", get)
	}
	if res := tool.Execute(ctx, map[string]any{"action": "get"}); !res.IsError || res.ForLLM != "job_id is required for get" {
		t.Errorf("get without id: %+v", res)
	}
	if res := tool.Execute(ctx, map[string]any{"action": "enable", "job_id": daily}); res.IsError || res.ForLLM != "Cron job 'daily' enabled" {
		t.Errorf("enable: %+v", res)
	}
}

func TestFormatSchedule_Kinds(t *testing.T) {
	ms := int64(90000)
	last := int64(0)
	for want, j := range map[string]*cron.CronJob{
		"every 90s":           {Schedule: cron.CronSchedule{Kind: "every", EveryMS: &ms}},
		"listen (continuous)": {Schedule: cron.CronSchedule{Kind: cron.KindListen}},
		"unknown":             {Schedule: cron.CronSchedule{Kind: "every"}},
	} {
		if got := formatSchedule(j); got != want {
			t.Errorf("formatSchedule = %q, want %q", got, want)
		}
	}
	tool := newTestCronTool(t)
	job, err := tool.cronService.AddJob("ran", cron.CronSchedule{Kind: "every", EveryMS: &ms}, "ran", "agent", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	job.AgentID = "alice"
	job.State.LastRunAtMS = &last
	if err := tool.cronService.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	if got := tool.Execute(agentCtx("alice"), map[string]any{"action": "get", "job_id": job.ID}).ForLLM; !strings.Contains(got, "  last run: ") {
		t.Errorf("get with a last run:\n%s", got)
	}
}

// An operator job without a peer kind is delivered to a channel peer.
func TestDeliver_OperatorDefaultPeerKind(t *testing.T) {
	tool := newTestCronTool(t)
	job := &cron.CronJob{ID: "op", Payload: cron.CronPayload{Message: "m", Channel: "slack", To: "C1"}}
	if out, err := tool.ExecuteJob(context.Background(), job); err != nil || out != "ok" {
		t.Fatalf("ExecuteJob: %q %v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10_000_000_000)
	defer cancel()
	msg, ok := tool.msgBus.ConsumeInbound(ctx)
	if !ok || msg.Peer.Kind != "channel" || msg.Peer.ID != "C1" || msg.Channel != "slack" {
		t.Errorf("delivered %+v", msg)
	}
}
