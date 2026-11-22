package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/providers"
)

// exempt reports whether name is registered as suite-exempt.
func exempt(r *ToolRegistry, name string) bool {
	info, ok := r.cat.Get(name)
	return ok && isSuiteExempt(info)
}

// These tests pin how ToolRegistry maps onto its toolkit catalogue, and the
// host rules it adds on top: the suite exemption, the per-call allow checker
// and who a call is for.

func TestWrapper_RegisterMapsEntries(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("core", "core tool"))
	r.RegisterHidden(newMockTool("hidden", "hidden tool"))
	r.RegisterHiddenGroup(newMockTool("grouped", "grouped tool"), "trello", true)
	r.RegisterSuite(newMockTool("suite", "suite tool"))
	r.RegisterSuiteHidden(newMockTool("suite_hidden", "hidden suite tool"))
	r.RegisterSuiteHiddenGroup(newMockTool("suite_grouped", "grouped suite tool"), "fusion", false)

	for _, tc := range []struct {
		name           string
		hidden, exempt bool
		group          string
		reveal         bool
	}{
		{name: "core"},
		{name: "hidden", hidden: true},
		{name: "grouped", hidden: true, group: "trello", reveal: true},
		{name: "suite", exempt: true},
		{name: "suite_hidden", hidden: true, exempt: true},
		{name: "suite_grouped", hidden: true, exempt: true, group: "fusion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := r.cat.Get(tc.name)
			if !ok {
				t.Fatalf("%s not in the catalogue", tc.name)
			}
			if info.Hidden != tc.hidden || info.Group != tc.group || info.RevealTogether != tc.reveal {
				t.Errorf("entry = hidden %v group %q reveal %v, want %v %q %v",
					info.Hidden, info.Group, info.RevealTogether, tc.hidden, tc.group, tc.reveal)
			}
			if got := exempt(r, tc.name); got != tc.exempt {
				t.Errorf("suite-exempt = %v, want %v", got, tc.exempt)
			}
		})
	}
}

// Re-registering a suite tool as an ordinary one drops its exemption.
func TestWrapper_ReRegisterClearsExemption(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterSuite(newMockTool("x", "suite"))
	r.Register(newMockTool("x", "plain"))
	if exempt(r, "x") {
		t.Fatal("x kept its suite exemption after an ordinary registration")
	}
}

func TestWrapper_ReplaceByPrefixMapsEntries(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterSuite(newMockTool("mcp_a_old", "old"))
	r.Register(newMockTool("other", "other"))

	removed := r.ReplaceByPrefix("mcp_a_", []ToolEntry{
		{Tool: newMockTool("mcp_a_listed", "listed"), IsCore: true},
		{Tool: newMockTool("mcp_a_deferred", "deferred"), Group: "a", RevealTogether: true},
		{Tool: newMockTool("mcp_a_suite", "suite"), IsCore: true, SuiteExempt: true},
	})
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if got, want := r.List(), []string{"mcp_a_deferred", "mcp_a_listed", "mcp_a_suite", "other"}; !slices.Equal(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	if info, _ := r.cat.Get("mcp_a_listed"); info.Hidden {
		t.Error("an IsCore entry must be listed")
	}
	if info, _ := r.cat.Get("mcp_a_deferred"); !info.Hidden || info.Group != "a" || !info.RevealTogether {
		t.Errorf("deferred entry = %+v, want hidden in reveal-together group a", info)
	}
	if exempt(r, "mcp_a_old") || !exempt(r, "mcp_a_suite") || exempt(r, "mcp_a_listed") {
		t.Error("want only mcp_a_suite suite-exempt")
	}

	if n := r.RemoveByPrefix("mcp_a_"); n != 3 {
		t.Errorf("RemoveByPrefix = %d, want 3", n)
	}
	if got := r.List(); !slices.Equal(got, []string{"other"}) {
		t.Errorf("List after removal = %v", got)
	}
}

func TestWrapper_ReadMethods(t *testing.T) {
	r := NewToolRegistry()
	v0 := r.Version()
	r.Register(newMockTool("b_core", "core"))
	r.RegisterHidden(&mockExternalTool{
		mockRegistryTool: *newMockTool("mcp_srv_find", "find things"),
		external:         "srv_find",
	})
	if r.Version() == v0 {
		t.Error("Version did not move on Register")
	}
	if r.Count() != 2 || !slices.Equal(r.List(), []string{"b_core", "mcp_srv_find"}) {
		t.Errorf("Count %d, List %v", r.Count(), r.List())
	}
	if tool, ok := r.Get("mcp_srv_find"); !ok || tool.Name() != "mcp_srv_find" {
		t.Error("Get by internal name failed for a hidden tool")
	}
	if _, ok := r.Get("srv_find"); ok {
		t.Error("Get must match the internal name only")
	}
	if defs := r.GetDefinitions(); len(defs) != 1 {
		t.Errorf("GetDefinitions = %d, want the 1 listed tool", len(defs))
	}
	if sums := r.GetSummaries(); len(sums) != 1 || sums[0] != "- `b_core` - core" {
		t.Errorf("GetSummaries = %v", sums)
	}

	defs := r.ToProviderDefs()
	if len(defs) != 2 {
		t.Fatalf("ToProviderDefs = %d defs, want 2", len(defs))
	}
	if defs[0].Function.Name != "b_core" || defs[0].Deferred {
		t.Errorf("defs[0] = %+v, want listed b_core", defs[0])
	}
	if defs[1].Function.Name != "srv_find" || !defs[1].Deferred {
		t.Errorf("defs[1] = %+v, want deferred srv_find", defs[1])
	}

	snap := r.SnapshotHiddenTools()
	if snap.Version != r.Version() || len(snap.Docs) != 1 || snap.Docs[0].Name != "srv_find" {
		t.Errorf("SnapshotHiddenTools = %+v", snap)
	}
	if ns := r.HiddenNamespaces(); !slices.Equal(ns, []string{"srv"}) {
		t.Errorf("HiddenNamespaces = %v", ns)
	}
}

// agentProbe is a toolspec provider whose one tool reports who its call was
// for.
type agentProbe struct{}

func (agentProbe) RegisterTools(global.Deps) []global.ToolDefinition {
	return []global.ToolDefinition{{
		Name:        "whoami",
		Description: "reports the caller",
		Handler: func(call *global.ToolCall) (*global.Result, error) {
			return &global.Result{ForLLM: fmt.Sprintf("agent=%s chat=%s/%s", call.AgentID, call.Channel, call.ChatID)}, nil
		},
	}}
}

// The toolspec bridge reads the agent id from toolkit's key: the registry
// copies the one the host's turn put on the context.
func TestWrapper_BridgedToolSeesAgent(t *testing.T) {
	r := NewToolRegistry()
	for _, tool := range NamespacedProvider("probe", agentProbe{}).Build(ToolDeps{}) {
		r.Register(tool)
	}
	ctx := providers.WithAgentID(context.Background(), "alice")

	res := r.ExecuteWithContext(ctx, "probe_whoami", nil, "telegram", "42", nil)
	if res.IsError || res.ForLLM != "agent=alice chat=telegram/42" {
		t.Errorf("ExecuteWithContext = %+v", res)
	}
	out, err := r.ExecuteUnattended(ctx, "probe_whoami", nil)
	if err != nil || out != "agent=alice chat=/" {
		t.Errorf("ExecuteUnattended = %q, %v", out, err)
	}
	if res := r.Execute(providers.WithAgentID(context.Background(), "bob"), "probe_whoami", nil); res.ForLLM != "agent=bob chat=/" {
		t.Errorf("Execute = %+v", res)
	}
}

func TestWrapper_ExecuteTexts(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("plain", "plain"))
	r.RegisterSuite(newMockTool("suite", "suite"))
	checker := mockAllowChecker{allowed: map[string]bool{}, denied: map[string]bool{"denied_suite": true}}
	r.RegisterSuite(newMockTool("denied_suite", "suite, denied"))
	ctx := WithToolAllowChecker(context.Background(), checker)

	t.Run("not found", func(t *testing.T) {
		res := r.ExecuteWithContext(ctx, "nope", nil, "", "", nil)
		if !res.IsError || res.ForLLM != `tool "nope" not found` {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("denied", func(t *testing.T) {
		res := r.ExecuteWithContext(ctx, "plain", nil, "", "", nil)
		if !res.IsError || res.ForLLM != NotEnabledMessage("plain") || !IsExpectedRefusal(res.Err) {
			t.Errorf("result = %+v", res)
		}
		if res.Err.Error() != "tool not permitted: plain" {
			t.Errorf("error = %q", res.Err)
		}
	})
	t.Run("suite exempt from the allow list", func(t *testing.T) {
		if res := r.ExecuteWithContext(ctx, "suite", nil, "", "", nil); res.IsError {
			t.Errorf("result = %+v, want the suite tool to run", res)
		}
	})
	t.Run("suite still denied", func(t *testing.T) {
		res := r.ExecuteWithContext(ctx, "denied_suite", nil, "", "", nil)
		if !res.IsError || res.ForLLM != NotEnabledMessage("denied_suite") {
			t.Errorf("result = %+v", res)
		}
	})
}

// A tool removed between the model's tool list and its call is an ordinary
// tool error, never a panic; a tool added mid-turn is in the next list.
func TestWrapper_AddRemoveMidTurn(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("keep", "keep"))
	r.Register(newMockTool("gone", "gone"))

	offered := r.ToProviderDefs() // the model's list for this call
	if len(offered) != 2 {
		t.Fatalf("offered %d tools, want 2", len(offered))
	}
	r.RemoveByPrefix("gone")
	r.Register(newMockTool("added", "added mid-turn"))

	res := r.ExecuteWithContext(context.Background(), "gone", nil, "", "", nil)
	if !res.IsError || res.ForLLM != `tool "gone" not found` || !errors.Is(res.Err, toolkit.ErrToolNotFound) {
		t.Errorf("call to a removed tool = %+v", res)
	}

	defs := r.ToProviderDefs()
	next := make([]string, 0, len(defs))
	for _, d := range defs {
		next = append(next, d.Function.Name)
	}
	if !slices.Equal(next, []string{"added", "keep"}) {
		t.Errorf("next list = %v, want [added keep]", next)
	}
}

// Adds, removes and calls from many goroutines at once: every call returns a
// result (run with -race).
func TestWrapper_ConcurrentChangeAndCall(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("stable", "stable"))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				name := fmt.Sprintf("dyn_%d_%d", i, j)
				r.RegisterHidden(newMockTool(name, "dynamic"))
				r.ReplaceByPrefix(fmt.Sprintf("dyn_%d_", i), []ToolEntry{{Tool: newMockTool(name, "again"), SuiteExempt: true}})
				r.RemoveByPrefix(name)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				if res := r.ExecuteWithContext(context.Background(), fmt.Sprintf("dyn_%d_%d", i, j), nil, "", "", nil); res == nil {
					t.Error("nil result")
				}
				_ = r.ToProviderDefs()
			}
		}(i)
	}
	wg.Wait()
	if !slices.Equal(r.List(), []string{"stable"}) {
		t.Errorf("List = %v, want [stable]", r.List())
	}
}

// ExecuteUnattended applies the context's allow checker and runs with no
// chat, whatever the caller's context carries.
func TestWrapper_ExecuteUnattendedChecksAndClearsChat(t *testing.T) {
	r := NewToolRegistry()
	for _, tool := range NamespacedProvider("probe", agentProbe{}).Build(ToolDeps{}) {
		r.Register(tool)
	}
	ctx := WithToolContext(context.Background(), "telegram", "42")

	out, err := r.ExecuteUnattended(ctx, "probe_whoami", nil)
	if err != nil || out != "agent= chat=/" {
		t.Errorf("allowed call = %q, %v; want the tool to see no chat", out, err)
	}

	denied := WithToolAllowChecker(ctx, mockAllowChecker{denied: map[string]bool{"probe_whoami": true}})
	out, err = r.ExecuteUnattended(denied, "probe_whoami", nil)
	want := `tool "probe_whoami" failed: ` + NotEnabledMessage("probe_whoami")
	if err == nil || err.Error() != want || out != "" {
		t.Errorf("denied call = %q, %v; want error %q", out, err, want)
	}
}

// countingTool counts its runs.
type countingTool struct {
	mockRegistryTool
	runs *atomic.Int64
}

func (c *countingTool) Execute(context.Context, map[string]any) *ToolResult {
	c.runs.Add(1)
	return SilentResult("ran")
}

// While a suite-exempt tool is swapped for an ordinary one of the same name
// under a checker that does not allow it, the ordinary one never runs: each
// call is checked against exactly the tool it runs (run with -race).
func TestWrapper_GateChecksTheToolThatRuns(t *testing.T) {
	r := NewToolRegistry()
	var exemptRuns, plainRuns atomic.Int64
	exemptTool := &countingTool{mockRegistryTool: *newMockTool("mcp_s_x", "exempt"), runs: &exemptRuns}
	plainTool := &countingTool{mockRegistryTool: *newMockTool("mcp_s_x", "plain"), runs: &plainRuns}
	ctx := WithToolAllowChecker(context.Background(), mockAllowChecker{})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r.ReplaceByPrefix("mcp_s_", []ToolEntry{{Tool: exemptTool, IsCore: true, SuiteExempt: true}})
			r.ReplaceByPrefix("mcp_s_", []ToolEntry{{Tool: plainTool, IsCore: true}})
		}
	}()
	var callers sync.WaitGroup
	for range 4 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for range 500 {
				r.ExecuteWithContext(ctx, "mcp_s_x", nil, "", "", nil)
				_, _ = r.ExecuteUnattended(ctx, "mcp_s_x", nil)
			}
		}()
	}
	callers.Wait()
	close(stop)
	wg.Wait()

	if n := plainRuns.Load(); n != 0 {
		t.Fatalf("the denied tool ran %d times", n)
	}
	if exemptRuns.Load() == 0 {
		t.Log("the exempt tool never ran; the swap always won the race")
	}
}
