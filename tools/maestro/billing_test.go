// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package maestro

import (
	"context"
	"testing"

	mllm "go.bison9.com/maestro/llm"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// billingStubRunner records whether a dispatch ran with an inherited code,
// and which.
type billingStubRunner struct {
	inherited bool
	code      string
}

func (r *billingStubRunner) RunSync(ctx context.Context, _, _ string) (*global.SyncResult, error) {
	r.code, r.inherited = tools.Billing(ctx)
	return &global.SyncResult{Content: "ok"}, nil
}

// A dispatched task is billed to its billing argument, else to the code of
// the work that dispatched it; the worker's code is its own, so a change it
// makes never reaches the dispatching work.
func TestIntegration_DispatchBilling(t *testing.T) {
	h := newMaestroHarness(t)
	const project = "billing"
	h.call("maestro_project_create", map[string]any{"name": project, "title": "Billing", "disclaimer_template": "none"}, false)
	h.runner.change = "worker-own"
	caller := tools.WithBilling(context.Background(), "acme")
	for _, tc := range []struct{ path, arg, want string }{
		{"inherited", "", "acme"},
		{"explicit", "pentest", "pentest"},
	} {
		args := map[string]any{"project": project, "path": tc.path, "prompt": "work"}
		if tc.arg != "" {
			args["billing"] = tc.arg
		}
		h.callCtx(caller, "maestro_task_dispatch", args)
		h.waitTerminal(project, tc.path)
		h.runner.mu.Lock()
		got := h.runner.billings[len(h.runner.billings)-1]
		h.runner.mu.Unlock()
		if got != tc.want {
			t.Errorf("%s: worker billed %q, want %q", tc.path, got, tc.want)
		}
	}
	if code, _ := tools.Billing(caller); code != "acme" {
		t.Errorf("the dispatching work's code = %q, want acme kept", code)
	}
}

// Without a code on the request or the context the worker has no inherited
// code (its own session's applies).
func TestDispatcher_NoBilling(t *testing.T) {
	r := &billingStubRunner{}
	d := &dispatcher{run: r}
	if _, err := d.Dispatch(context.Background(), &mllm.DispatchRequest{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if r.inherited {
		t.Error("a request without a code ran with one")
	}
}

// A task code that is not one (a hand-edited task set) runs under the
// default instead of failing every attempt.
func TestDispatcher_InvalidBilling(t *testing.T) {
	r := &billingStubRunner{}
	d := &dispatcher{run: r}
	if _, err := d.Dispatch(context.Background(), &mllm.DispatchRequest{Prompt: "x", Billing: "not a code"}); err != nil {
		t.Fatal(err)
	}
	if r.code != global.DefaultBilling {
		t.Errorf("worker billed %q, want the default", r.code)
	}
}
