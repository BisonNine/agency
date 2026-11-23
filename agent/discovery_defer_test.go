// ClawEh
// License: MIT

package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// notebookTool is a hidden tool the discovery tests find and call.
type notebookTool struct {
	calls atomic.Int32
}

func (n *notebookTool) Name() string        { return "mcp_alice_notebook" }
func (n *notebookTool) Description() string { return "Read a page of Alice's notebook" }
func (n *notebookTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (n *notebookTool) Execute(context.Context, map[string]any) *toolkit.Result {
	n.calls.Add(1)
	return toolkit.SilentResult("page 1: buy milk")
}

// defsRecordingProvider answers from a script and records the tool definitions
// and messages of every request.
type defsRecordingProvider struct {
	mu        sync.Mutex
	responses []*spawnllm.LLMResponse
	defs      [][]spawnllm.ToolDefinition
	messages  [][]spawnllm.Message
}

func (p *defsRecordingProvider) Chat(_ context.Context, _ accounting.Identity, messages []spawnllm.Message, defs []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defs = append(p.defs, slices.Clone(defs))
	p.messages = append(p.messages, slices.Clone(messages))
	i := len(p.defs) - 1
	if i >= len(p.responses) {
		return &spawnllm.LLMResponse{Content: "done"}, nil
	}
	return p.responses[i], nil
}

func (p *defsRecordingProvider) GetDefaultModel() string { return "test" }

func toolCallResponse(id, name, args string) *spawnllm.LLMResponse {
	return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
		ID: id, Type: "function", Name: name,
		Function: &spawnllm.FunctionCall{Name: name, Arguments: args},
	}}}
}

// toolMessageFor returns the tool message answering callID in msgs.
func toolMessageFor(msgs []spawnllm.Message, callID string) (spawnllm.Message, bool) {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == callID {
			return m, true
		}
	}
	return spawnllm.Message{}, false
}

// A turn in which the model searches, then calls the hidden tool it found:
// every request offers the same tool definitions (the hidden tool deferred,
// search_tools as the tool-search tool), the search result carries the hit as
// a tool reference into the next request and into the stored session, and
// the hidden tool runs.
func TestDiscovery_TurnSendsSameToolsAndRunsFoundTool(t *testing.T) {
	al := newTestAgentLoop(t).al
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	notebook := &notebookTool{}
	registerHiddenSuite(agent, notebook)
	agent.Tools.RegisterDiscoveryTools(context.Background(), 5, nil)

	provider := &defsRecordingProvider{responses: []*spawnllm.LLMResponse{
		toolCallResponse("tc-search", "search_tools", `{"query":"Alice notebook page"}`),
		toolCallResponse("tc-read", "mcp_alice_notebook", `{}`),
		{Content: "Alice's notebook says to buy milk."},
	}}
	setTestProvider(agent, provider)

	const key = "agent:main:discovery-defer"
	opts := processOptions{
		SessionKey:      key,
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "What is on Alice's notebook page?",
		DefaultResponse: defaultResponse,
	}
	got, err := al.runAgentLoop(context.Background(), agent, opts)
	if err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if got != "Alice's notebook says to buy milk." {
		t.Errorf("answer = %q", got)
	}
	if n := notebook.calls.Load(); n != 1 {
		t.Errorf("hidden tool ran %d times, want 1", n)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.defs) != 3 {
		t.Fatalf("provider called %d times, want 3", len(provider.defs))
	}
	for i := 1; i < len(provider.defs); i++ {
		if !reflect.DeepEqual(provider.defs[i], provider.defs[0]) {
			t.Fatalf("request %d offered different tools than request 0:\n got %+v\nwant %+v", i, provider.defs[i], provider.defs[0])
		}
	}
	var sawNotebook, sawSearch bool
	for _, d := range provider.defs[0] {
		switch d.Function.Name {
		case "mcp_alice_notebook":
			sawNotebook = true
			if !d.Deferred {
				t.Error("the hidden tool is not offered deferred")
			}
		case "search_tools":
			sawSearch = true
			if !d.ToolSearch || d.Deferred {
				t.Errorf("search_tools: ToolSearch=%v Deferred=%v", d.ToolSearch, d.Deferred)
			}
		}
	}
	if !sawNotebook || !sawSearch {
		t.Fatalf("tool list lacks the hidden tool (%v) or search_tools (%v)", sawNotebook, sawSearch)
	}

	want := []string{"mcp_alice_notebook"}
	msg, ok := toolMessageFor(provider.messages[1], "tc-search")
	if !ok {
		t.Fatal("second request lacks the search result")
	}
	if !slices.Equal(msg.ToolReferences, want) {
		t.Errorf("search result references in the request = %v, want %v", msg.ToolReferences, want)
	}

	stored, ok := toolMessageFor(agent.Sessions.GetHistory(key), "tc-search")
	if !ok {
		t.Fatal("stored session lacks the search result")
	}
	if !slices.Equal(stored.ToolReferences, want) {
		t.Errorf("stored search result references = %v, want %v", stored.ToolReferences, want)
	}
	if read, ok := toolMessageFor(agent.Sessions.GetHistory(key), "tc-read"); !ok || len(read.ToolReferences) != 0 {
		t.Errorf("stored hidden-tool result = %+v (found %v), want no references", read, ok)
	}
}

// The session store keeps a tool message's references across a close and
// reopen.
func TestSessionStore_KeepsToolReferences(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	modes := config.Modes{Dir: 0o700, File: 0o600}
	const key = "agent:main:refs"

	store, err := initSessionStore(dir, modes)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []spawnllm.Message{
		{Role: "user", Content: "find Alice's notebook"},
		{Role: "assistant", ToolCalls: []spawnllm.ToolCall{{
			ID: "tc-1", Type: "function", Name: "search_tools",
			Function: &spawnllm.FunctionCall{Name: "search_tools", Arguments: `{"query":"notebook"}`},
		}}},
		{Role: "tool", ToolCallID: "tc-1", Content: "Found 1 tool(s)", ToolReferences: []string{"mcp_alice_notebook"}},
	} {
		if _, addErr := store.AddFullMessage(key, m); addErr != nil {
			t.Fatalf("AddFullMessage: %v", addErr)
		}
	}
	closeT(t, store)

	reopened, err := initSessionStore(dir, modes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeT(t, reopened) })
	msg, ok := toolMessageFor(reopened.GetHistory(key), "tc-1")
	if !ok {
		t.Fatal("tool message not stored")
	}
	if want := []string{"mcp_alice_notebook"}; !slices.Equal(msg.ToolReferences, want) {
		t.Errorf("ToolReferences after reopen = %v, want %v", msg.ToolReferences, want)
	}
}

// bulkyTool is a hidden tool with a long description, so leaving it out of
// an estimate is visible.
type bulkyTool struct{ name string }

func (b bulkyTool) Name() string { return b.name }
func (b bulkyTool) Description() string {
	return "Bob's upstream tool. " + strings.Repeat("It does many things. ", 40)
}

func (b bulkyTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (b bulkyTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return toolkit.SilentResult("ok")
}

// The tool-schema cost a turn puts on the context manager's requests counts
// only what the request sends: with discovery on and nothing referenced, the
// hidden tools cost nothing; once a search result in the turn references
// some, the next request counts them, and a later turn over the stored
// history counts them from its first request.
func TestTurn_EstimatesOnlySentTools(t *testing.T) {
	al := newTestAgentLoop(t).al
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	agent.Tools.RegisterDiscoveryTools(context.Background(), 5, nil)
	for i := range 200 {
		agent.Tools.RegisterEntries(tools.ToolEntry{Entry: discovery.HideEntries([]toolkit.Entry{{Tool: bulkyTool{name: fmt.Sprintf("mcp_bob_tool_%03d", i)}}})[0]})
	}
	var coreNow []spawnllm.ToolDefinition
	hidden := map[string]spawnllm.ToolDefinition{}
	for _, d := range agent.Tools.ToolDefs(nil).Send {
		if d.Deferred {
			hidden[d.Function.Name] = d
		} else {
			coreNow = append(coreNow, d)
		}
	}
	coreTokens := ctxengine.EstimateToolDefinitionTokens(coreNow)
	withRefs := func(refs []string) int {
		defs := slices.Clone(coreNow)
		for _, name := range refs {
			d, ok := hidden[name]
			if !ok {
				t.Fatalf("referenced tool %q is not a hidden tool", name)
			}
			d.Deferred = false
			defs = append(defs, d)
		}
		return ctxengine.EstimateToolDefinitionTokens(defs)
	}

	provider := &defsRecordingProvider{responses: []*spawnllm.LLMResponse{
		toolCallResponse("tc-1", "search_tools", `{"query":"mcp_bob_tool_007"}`),
		{Content: "Found Bob's tool."},
	}}
	setTestProvider(agent, provider)

	const key = "agent:main:estimate"
	cm, release := al.getContextManager(agent, key)
	defer release()
	opts := processOptions{SessionKey: key, Channel: "cli", ChatID: "direct", UserMessage: "find Bob's tool"}
	rec := &assemblyRecorder{ContextManager: cm}
	if _, _, err := testTurn(t, al, agent, opts, al.selectTurnModel(agent, key), rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if len(provider.messages) != 2 {
		t.Fatalf("model calls = %d, want 2", len(provider.messages))
	}
	found, ok := toolMessageFor(provider.messages[1], "tc-1")
	if !ok || len(found.ToolReferences) == 0 {
		t.Fatalf("the search result referenced no tool: %+v", found)
	}
	wantRefs := withRefs(found.ToolReferences)
	if wantRefs <= coreTokens {
		t.Fatalf("precondition: the referenced tools must cost something (%d <= %d)", wantRefs, coreTokens)
	}
	// Each request is assembled before its call; the first request's
	// assembly comes first.
	if got := rec.tokens(); len(got) < 2 || got[0] != coreTokens || got[len(got)-1] != wantRefs {
		t.Errorf("tool tokens per assembly = %v, want %d first (the core tools) and %d last (core plus the referenced tools)", got, coreTokens, wantRefs)
	}

	opts.UserMessage = "use it"
	rec = &assemblyRecorder{ContextManager: cm}
	if _, _, err := testTurn(t, al, agent, opts, al.selectTurnModel(agent, key), rec); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if got := rec.tokens(); len(got) == 0 || got[0] != wantRefs {
		t.Errorf("next turn's first request: tool tokens = %v, want %d first (the stored references counted)", got, wantRefs)
	}
}

// registerHiddenSuite registers t on agent as a discovery-hidden suite tool,
// as registerProviderTools does for fusion and maestro.
func registerHiddenSuite(agent *AgentInstance, t toolkit.Tool) {
	for _, e := range discovery.HideEntries([]toolkit.Entry{{Tool: t}}) {
		agent.Tools.RegisterEntries(tools.ToolEntry{Entry: e, SuiteExempt: true})
	}
}
