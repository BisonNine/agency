package tools

import (
	"context"
	"fmt"
	"testing"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

// fakeGlobalProvider returns bare-named tools for bridge testing.
type fakeGlobalProvider struct{}

func (fakeGlobalProvider) RegisterTools(deps toolspec.Deps) []toolspec.ToolDefinition {
	return []toolspec.ToolDefinition{
		{
			Name:        "read",
			Description: "read a thing",
			Parameters: []toolspec.Parameter{
				{Name: "path", Type: "string", Required: true, Description: "the path"},
			},
			DefaultAllow: toolspec.Allow(true),
			Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
				path, ok := call.Args["path"].(string)
				if !ok {
					return nil, fmt.Errorf("path is %T, want string", call.Args["path"])
				}
				return &toolspec.Result{ForLLM: "read:" + path + " sess=" + call.Session}, nil
			},
		},
		{
			Name:          "watch",
			Description:   "watch a thing",
			SessionScoped: true,
			Async:         true,
			Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
				if call.Notify != nil {
					call.Notify(&toolspec.Result{ForLLM: "done"})
				}
				return &toolspec.Result{ForLLM: "started", Async: true}, nil
			},
		},
	}
}

func TestNamespacedProvider_PrefixesNames(t *testing.T) {
	p := NamespacedProvider("file", fakeGlobalProvider{})
	built := p.Build(ToolDeps{})
	got := map[string]bool{}
	for _, tl := range built {
		got[tl.Name()] = true
	}
	if !got["file_read"] || !got["file_watch"] {
		t.Fatalf("expected file_read and file_watch, got %v", got)
	}
}

func TestNamespacedProvider_ExecuteAndSchema(t *testing.T) {
	p := NamespacedProvider("file", fakeGlobalProvider{})
	var readTool toolkit.Tool
	for _, tl := range p.Build(ToolDeps{}) {
		if tl.Name() == "file_read" {
			readTool = tl
		}
	}
	if readTool == nil {
		t.Fatal("file_read not built")
	}
	// Schema generated from []Parameter.
	schema := readTool.Parameters()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map in schema, got %v", schema)
	}
	if _, ok := props["path"]; !ok {
		t.Fatalf("expected 'path' property in schema, got %v", schema)
	}
	// Session is threaded from ctx into the call.
	ctx := toolkit.WithSessionKey(context.Background(), "sess-1")
	res := readTool.Execute(ctx, map[string]any{"path": "/x"})
	if res.IsError || res.ForLLM != "read:/x sess=sess-1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestNamespacedProvider_AsyncAndSessionInterfaces(t *testing.T) {
	p := NamespacedProvider("file", fakeGlobalProvider{})
	var watch toolkit.Tool
	for _, tl := range p.Build(ToolDeps{}) {
		if tl.Name() == "file_watch" {
			watch = tl
		}
	}
	if watch == nil {
		t.Fatal("file_watch not built")
	}
	// def flags must surface as the optional interfaces.
	ss, ok := watch.(toolkit.SessionScoped)
	if !ok || !ss.IsSessionScoped() {
		t.Fatal("file_watch should be SessionScoped")
	}
	ae, ok := watch.(toolkit.AsyncExecutor)
	if !ok {
		t.Fatal("file_watch should be AsyncExecutor")
	}
	var got *toolkit.Result
	res := ae.ExecuteAsync(context.Background(), map[string]any{}, func(_ context.Context, r *toolkit.Result) { got = r })
	if !res.Async {
		t.Fatalf("expected async result, got %+v", res)
	}
	if got == nil || got.ForLLM != "done" {
		t.Fatalf("expected notify callback to deliver 'done', got %+v", got)
	}
	// The non-async, non-session tool must NOT satisfy those interfaces.
	var read toolkit.Tool
	for _, tl := range p.Build(ToolDeps{}) {
		if tl.Name() == "file_read" {
			read = tl
		}
	}
	if _, ok := read.(toolkit.AsyncExecutor); ok {
		t.Fatal("file_read must not be AsyncExecutor")
	}
	if _, ok := read.(toolkit.SessionScoped); ok {
		t.Fatal("file_read must not be SessionScoped")
	}
}

func TestNamespacedProvider_Describe(t *testing.T) {
	p := NamespacedProvider("file", fakeGlobalProvider{})
	descs := p.Describe()
	var readDesc *ToolDescriptor
	for i := range descs {
		if descs[i].Name == "file_read" {
			readDesc = &descs[i]
		}
	}
	if readDesc == nil || !readDesc.DefaultEnabled {
		t.Fatalf("expected file_read descriptor with DefaultEnabled, got %+v", descs)
	}
}

// denyByDefaultProvider returns one tool that explicitly does not default to
// allowed, and one that leaves DefaultAllow unset.
type denyByDefaultProvider struct{}

func (denyByDefaultProvider) RegisterTools(toolspec.Deps) []toolspec.ToolDefinition {
	handler := func(*toolspec.ToolCall) (*toolspec.Result, error) { return &toolspec.Result{ForLLM: "ok"}, nil }
	return []toolspec.ToolDefinition{
		{Name: "purge", Description: "purge a thing", DefaultAllow: toolspec.Allow(false), Handler: handler},
		{Name: "probe", Description: "probe a thing", Handler: handler},
	}
}

// A namespaced tool whose DefaultAllow is false is still built when deps.Cfg
// is set: DefaultAllow only feeds the "Default tools" list, and which tools an
// agent gets is decided by its own tools list when they are registered, not
// by a per-tool filter at build time.
func TestNamespacedProvider_BuildIgnoresDefaultAllow(t *testing.T) {
	p := NamespacedProvider("thing", denyByDefaultProvider{})
	for _, d := range p.Describe() {
		if d.DefaultEnabled {
			t.Fatalf("descriptor %s DefaultEnabled = true, want false", d.Name)
		}
	}
	cfg := &config.Config{Agents: config.AgentsConfig{List: []config.AgentConfig{{ID: "alice", Default: true}}}}
	got := map[string]bool{}
	for _, tl := range p.Build(ToolDeps{Cfg: cfg, AgentID: "alice"}) {
		got[tl.Name()] = true
	}
	if !got["thing_purge"] || !got["thing_probe"] || len(got) != 2 {
		t.Fatalf("built = %v, want thing_purge and thing_probe", got)
	}
}
