// Agency
// License: MIT

package mcpserver

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// TestDispatch_TalliesSubagentToolCalls: every MCP call made under a verified
// sub-agent session token is counted in that run's tool tally (CLI providers
// reach their tools this way): calls that ran, and calls refused after the
// token was verified (tool not in the registry, denied by the ACL).
func TestDispatch_TalliesSubagentToolCalls(t *testing.T) {
	const session = "agent:clone-tally:main"
	ok := &mockTool{name: "read_file", params: map[string]any{}, result: &toolkit.Result{ForLLM: "contents"}}
	bad := &mockTool{name: "write_file", params: map[string]any{}, result: &toolkit.Result{ForLLM: "disk full", IsError: true}}
	denied := &mockTool{name: "shell_exec", params: map[string]any{}, result: &toolkit.Result{ForLLM: "ran"}}
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(ok, bad, denied)}
	policy := acl.PolicyFunc(func(_, tool string) bool { return tool != "shell_exec" })
	st := newSessionTokenStore()
	tok := st.Issue("alice", session, "/ws/alice/sessions")

	dispatch := func(name string) {
		t.Helper()
		dispatchToolCall(context.Background(), name,
			map[string]any{"session_token": tok}, st, resolverFor(regs), nil, policy, nil, nil)
	}

	tools.BeginToolStats(session)
	defer tools.EndToolStats(session)

	dispatch("read_file")
	dispatch("write_file")
	calls, errs, last := tools.EndToolStats(session)
	if calls != 2 || errs != 1 || last != "write_file: disk full" {
		t.Errorf("tally = %d calls, %d errors, last %q; want 2, 1, %q", calls, errs, last, "write_file: disk full")
	}

	tools.BeginToolStats(session)
	dispatch("no_such_tool")
	calls, errs, last = tools.EndToolStats(session)
	if calls != 1 || errs != 1 || last != "no_such_tool: "+toolkit.NotEnabledMessage("no_such_tool") {
		t.Errorf("not-in-registry: %d calls, %d errors, last %q", calls, errs, last)
	}

	tools.BeginToolStats(session)
	dispatch("shell_exec")
	calls, errs, last = tools.EndToolStats(session)
	if calls != 1 || errs != 1 || !strings.HasPrefix(last, "shell_exec: ") {
		t.Errorf("acl_denied: %d calls, %d errors, last %q", calls, errs, last)
	}
	if denied.calls != 0 {
		t.Error("the denied tool ran")
	}
}

// panickingMock panics when executed.
type panickingMock struct{ mockTool }

func (*panickingMock) Execute(context.Context, map[string]any) *toolkit.Result {
	panic("tool blew up")
}

// TestDispatch_PanickingToolTalliedAsFailure: a tool that panics is counted as
// a failed call, and the panic still reaches the caller's recovery.
func TestDispatch_PanickingToolTalliedAsFailure(t *testing.T) {
	const session = "agent:clone-panic:main"
	pm := &panickingMock{mockTool{name: "read_file", params: map[string]any{}}}
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(pm)}
	st := newSessionTokenStore()
	tok := st.Issue("alice", session, "/ws/alice/sessions")

	tools.BeginToolStats(session)
	defer tools.EndToolStats(session)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the tool's panic did not propagate")
			}
		}()
		dispatchToolCall(context.Background(), "read_file",
			map[string]any{"session_token": tok}, st, resolverFor(regs), nil, nil, nil, nil)
	}()

	calls, errs, last := tools.EndToolStats(session)
	if calls != 1 || errs != 1 || last != "read_file: tool read_file panicked" {
		t.Errorf("tally = %d calls, %d errors, last %q; want 1, 1, the panic", calls, errs, last)
	}
}

// TestDispatch_PanickingHostCallbackTalliedAsFailure: a panic in one of the
// host's callbacks during a call (the ACL in ToolContext, the breadcrumb in
// BeforeTool, the tool-output switch in AfterTool) is answered with MCP's
// internal error and counted once, as a failure, as a panicking tool is.
func TestDispatch_PanickingHostCallbackTalliedAsFailure(t *testing.T) {
	boom := func() { panic("host callback blew up") }
	cases := []struct {
		name     string
		ctx      context.Context
		policy   acl.Policy
		activity ToolActivityNotifier
	}{
		{
			name: "ToolContext", ctx: context.Background(),
			policy: acl.PolicyFunc(func(_, _ string) bool { boom(); return true }),
		},
		{
			name: "BeforeTool", ctx: context.Background(),
			activity: func(_, _, _ string, _ map[string]any) string { boom(); return "" },
		},
		{name: "AfterTool", ctx: withToolOutput(context.Background(), func() bool { boom(); return true })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := "agent:clone-cbpanic-" + tc.name + ":main"
			rf := &mockTool{name: "read_file", params: map[string]any{}, result: &toolkit.Result{ForLLM: "contents"}}
			regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(rf)}
			st := newSessionTokenStore()
			tok := st.Issue("alice", session, "/ws/alice/sessions")

			tools.BeginToolStats(session)
			defer tools.EndToolStats(session)
			func() {
				defer func() {
					if _, ok := recover().(errPanicked); !ok {
						t.Error("the call was not answered with MCP's internal error (-32603)")
					}
				}()
				dispatchToolCall(tc.ctx, "read_file",
					map[string]any{"session_token": tok}, st, resolverFor(regs), nil, tc.policy, nil, tc.activity)
			}()

			calls, errs, last := tools.EndToolStats(session)
			if calls != 1 || errs != 1 || last != "read_file: tool read_file panicked" {
				t.Errorf("tally = %d calls, %d errors, last %q; want 1, 1, the panic", calls, errs, last)
			}
		})
	}
}

// TestDispatch_UnverifiedTokenNotTallied: a call whose session token cannot
// be resolved has no session to attribute it to and is not counted.
func TestDispatch_UnverifiedTokenNotTallied(t *testing.T) {
	const session = "agent:clone-unverified:main"
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(&mockTool{name: "read_file", params: map[string]any{}})}
	st := newSessionTokenStore()
	st.Issue("alice", session, "/ws/alice/sessions")

	tools.BeginToolStats(session)
	dispatchToolCall(context.Background(), "read_file",
		map[string]any{"session_token": "SST" + strings.Repeat("0", 63) + "1"}, st, resolverFor(regs), nil, nil, nil, nil)
	if calls, _, _ := tools.EndToolStats(session); calls != 0 {
		t.Errorf("an unverified call was tallied (%d calls)", calls)
	}
}
