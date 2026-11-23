package tools

import (
	"context"
	"fmt"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
)

// mockTool is a simple tool for registry tests.
type mockTool struct {
	name   string
	result *toolkit.Result
}

func (t *mockTool) Name() string               { return t.name }
func (t *mockTool) Description() string        { return "mock tool " + t.name }
func (t *mockTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t *mockTool) Execute(ctx context.Context, args map[string]any) *toolkit.Result {
	if t.result != nil {
		return t.result
	}
	return toolkit.NewResult("tool executed: " + t.name)
}

func TestToolRegistry_ToolDefs_WithTools(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "tool_x"})

	defs := r.ToolDefs(nil).Send
	if len(defs) != 1 {
		t.Fatalf("ToolDefs len = %d, want 1", len(defs))
	}
	if defs[0].Function.Name != "tool_x" {
		t.Errorf("Function.Name = %q, want tool_x", defs[0].Function.Name)
	}
}

func TestToolRegistry_ListSorted(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "z_tool"})
	r.Register(&mockTool{name: "a_tool"})

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
	// should be sorted
	if list[0] != "a_tool" || list[1] != "z_tool" {
		t.Errorf("List() = %v, want [a_tool z_tool]", list)
	}
}

func TestToolRegistry_CountWithRegistrations(t *testing.T) {
	r := NewToolRegistry()
	if r.Count() != 0 {
		t.Errorf("Count() = %d, want 0", r.Count())
	}
	r.Register(&mockTool{name: "t1_unique"})
	r.Register(&mockTool{name: "t2_unique"})
	if r.Count() != 2 {
		t.Errorf("Count() = %d, want 2", r.Count())
	}
}

func TestToolRegistry_HiddenTool_Gettable(t *testing.T) {
	r := NewToolRegistry()
	registerHidden(r, &mockTool{name: "hidden"})

	tool, ok := r.Get("hidden")
	if !ok {
		t.Fatal("a hidden tool must be resolvable without any discovery call")
	}
	if tool.Name() != "hidden" {
		t.Errorf("Name() = %q, want hidden", tool.Name())
	}
}

func TestToolRegistry_Execute_NotFoundExtra(t *testing.T) {
	r := NewToolRegistry()
	result := r.Execute(context.Background(), "absolutely_nonexistent_xyz", nil)
	if !result.IsError {
		t.Error("Execute() of nonexistent tool should return error")
	}
}

func TestToolRegistry_Execute_FoundWithResult(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "do_thing_unique", result: toolkit.NewResult("done")})

	result := r.Execute(context.Background(), "do_thing_unique", nil)
	if result.IsError {
		t.Fatalf("Execute() error: %s", result.ForLLM)
	}
	if result.ForLLM != "done" {
		t.Errorf("result = %q, want done", result.ForLLM)
	}
}

func TestToolRegistry_Version_IncreasesOnRegister(t *testing.T) {
	r := NewToolRegistry()
	v0 := r.Version()
	r.Register(&mockTool{name: "v1"})
	v1 := r.Version()
	if v1 <= v0 {
		t.Errorf("Version should increase after Register: v0=%d, v1=%d", v0, v1)
	}
}

// TestToolRegistry_ShellIsAnOrdinaryTool: an unregistered shell_exec gets the
// same not-found result as any other unregistered tool; one registered but
// hidden by progressive discovery runs, like any other hidden tool.
func TestToolRegistry_ShellIsAnOrdinaryTool(t *testing.T) {
	ctx := context.Background()
	missing := NewToolRegistry()
	for _, name := range []string{config.ShellExecTool, "file_write"} {
		res := missing.Execute(ctx, name, nil)
		if want := fmt.Sprintf("tool %q not found", name); !res.IsError || res.ForLLM != want {
			t.Errorf("unregistered %s: %+v, want %q", name, res, want)
		}
	}

	hidden := NewToolRegistry()
	registerHidden(hidden, &mockTool{name: config.ShellExecTool})
	res := hidden.Execute(ctx, config.ShellExecTool, nil)
	if res.IsError || res.ForLLM != "tool executed: "+config.ShellExecTool {
		t.Errorf("hidden: %+v, want it run", res)
	}
}
