// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package schedule

import (
	"context"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/cron"
	"go.bison9.com/agency/tools"
)

// firedBilling fires job and returns the billing code its message carries.
func firedBilling(t *testing.T, tool *CronTool, job *cron.CronJob) string {
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
	return msg.Metadata[bus.MetaBilling]
}

// A job is billed to the code in force where it was created, stored outside
// its fingerprinted payload; a job created before billing codes runs under
// the default.
func TestCronTool_JobBilling(t *testing.T) {
	tool := newTestCronTool(t)
	tool.SetBilling(func(context.Context) string { return "acme" })
	add := func(msg string) cron.CronJob {
		t.Helper()
		if res := tool.Execute(agentCtx("alice"), map[string]any{"action": "add", "message": msg, "at_seconds": float64(600)}); res.IsError {
			t.Fatalf("add: %s", res.ForLLM)
		}
		for _, j := range tool.cronService.ListJobs(true) {
			if j.Payload.Message == msg {
				return j
			}
		}
		t.Fatalf("job %q not found", msg)
		return cron.CronJob{}
	}
	job := add("stretch")
	if job.Billing != "acme" {
		t.Fatalf("job billed %q, want acme", job.Billing)
	}
	if got := firedBilling(t, tool, &job); got != "acme" {
		t.Errorf("fired job carries %q, want acme", got)
	}
	tool.SetBilling(func(context.Context) string { return "garden" })
	other := add("stretch")
	if other.Fingerprint != job.Fingerprint {
		t.Errorf("the billing code changed the fingerprint: %s vs %s", other.Fingerprint, job.Fingerprint)
	}
	old := job
	old.Billing = ""
	if got := firedBilling(t, tool, &old); got != "default" {
		t.Errorf("job without a code carries %q, want default", got)
	}
}

// Without a resolver the job takes the inherited code, else the default.
func TestCronTool_CallerBillingWithoutResolver(t *testing.T) {
	tool := newTestCronTool(t)
	if got := tool.callerBilling(context.Background()); got != "default" {
		t.Errorf("nothing inherited = %q", got)
	}
	if got := tool.callerBilling(tools.WithBilling(context.Background(), "acme")); got != "acme" {
		t.Errorf("inherited = %q", got)
	}
}

// A job whose stored code is not one (a hand-edited file) runs under the
// default instead of failing its turn.
func TestCronTool_InvalidStoredBilling(t *testing.T) {
	tool := newTestCronTool(t)
	tool.SetBilling(func(context.Context) string { return "acme" })
	if res := tool.Execute(agentCtx("alice"), map[string]any{"action": "add", "message": "m", "at_seconds": float64(600)}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	job := tool.cronService.ListJobs(true)[0]
	job.Billing = "not a code"
	if got := firedBilling(t, tool, &job); got != "default" {
		t.Errorf("invalid stored code fired with %q, want default", got)
	}
}

// A watch probe runs the agent's tool under the job's billing code.
func TestWatchProbe_BilledToTheJob(t *testing.T) {
	f := newWatchFixture(t)
	jobs := f.tool.cronService.ListJobs(true)
	job := jobs[0]
	job.Billing = "acme"
	if err := f.tool.cronService.UpdateJob(&job); err != nil {
		t.Fatal(err)
	}
	f.fire()
	if f.probe.sawCtx == nil {
		t.Fatal("probe not called")
	}
	if code, ok := tools.Billing(f.probe.sawCtx); !ok || code != "acme" {
		t.Errorf("probe billed %q (%v), want acme", code, ok)
	}
}

// A listener's calls run under the job's billing code.
func TestListen_BilledToTheJob(t *testing.T) {
	tool := &scriptedTool{replies: []scriptedReply{{text: `{"event":{"id":"e1"}}`}}}
	ct, msgBus := newListenEnv(t, tool)
	ct.SetBilling(func(context.Context) string { return "acme" })
	addListenJob(t, ct, nil)
	ct.StartListeners(context.Background())
	defer ct.StopListeners()
	msg, ok := nextInbound(t, msgBus, 3*time.Second)
	if !ok {
		t.Fatal("event not delivered")
	}
	if msg.Metadata[bus.MetaBilling] != "acme" {
		t.Errorf("event carries %q, want acme", msg.Metadata[bus.MetaBilling])
	}
	tool.mu.Lock()
	first := tool.billings[0]
	tool.mu.Unlock()
	if first != "acme" {
		t.Errorf("listener call billed %q, want acme", first)
	}
}
