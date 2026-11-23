package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
)

// --- the per-call grant check ---

// CallGate refuses a denied tool (suite-exempt or not) and a tool the agent
// does not allow unless it is suite-exempt; with no checker every tool runs.
func TestCallGate_Grants(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("plain", "p"))
	r.RegisterSuite(newMockTool("suite_tool", "s"))
	r.Register(newMockTool("allowed", "a"))
	info := func(name string) toolkit.Info {
		t.Helper()
		i, ok := r.cat.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		return i
	}
	checker := mockAllowChecker{
		allowed: map[string]bool{"allowed": true},
		denied:  map[string]bool{"denied_suite": true},
	}
	r.RegisterSuite(newMockTool("denied_suite", "d"))
	ctx := WithToolAllowChecker(context.Background(), checker)

	for name, refused := range map[string]bool{
		"plain":        true,  // not allowed, not exempt
		"suite_tool":   false, // the suite flag is the allow decision
		"allowed":      false,
		"denied_suite": true, // deny wins even for a suite tool
	} {
		res := CallGate(ctx, info(name))
		if got := res != nil; got != refused {
			t.Errorf("%s: refused=%v, want %v", name, got, refused)
			continue
		}
		if refused && (!res.IsError || !toolkit.IsExpectedRefusal(res.Err) || res.ForLLM != toolkit.NotEnabledMessage(name)) {
			t.Errorf("%s: refusal %+v", name, res)
		}
	}
	if res := CallGate(context.Background(), info("plain")); res != nil {
		t.Errorf("no checker: %+v", res)
	}
}

// A view offers only what the checker allows.
func TestView_OffersOnlyAllowed(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("alpha", "a"))
	r.Register(newMockTool("beta", "b"))
	view := r.View(mockAllowChecker{allowed: map[string]bool{"alpha": true}})
	names := view.Snapshot().Names()
	if len(names) != 1 || names[0] != "alpha" {
		t.Errorf("view offers %v", names)
	}
	if _, ok := view.Get("beta"); ok {
		t.Error("a tool the view does not grant is reachable")
	}
}

// CallContext carries the chat and the turn's agent id.
func TestCallContext_CarriesChatAndAgent(t *testing.T) {
	ctx := CallContext(spawnllm.WithAgentID(context.Background(), "alice"), "telegram", "chat-9")
	if toolkit.Channel(ctx) != "telegram" || toolkit.ChatID(ctx) != "chat-9" || toolkit.AgentID(ctx) != "alice" {
		t.Errorf("channel %q chat %q agent %q", toolkit.Channel(ctx), toolkit.ChatID(ctx), toolkit.AgentID(ctx))
	}
}

// --- registration ---

func TestRegistry_SnapshotSubscribeAndRefusal(t *testing.T) {
	r := NewToolRegistry()
	changes := make(chan toolkit.Change, 8)
	unsubscribe := r.Subscribe(func(ch toolkit.Change) { changes <- ch })
	defer unsubscribe()

	r.Register(newMockTool("one", "1"))
	// Notices are delivered on the catalogue's own goroutine.
	select {
	case ch := <-changes:
		if len(ch.Added) != 1 || ch.Added[0] != "one" {
			t.Errorf("change = %+v", ch)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subscriber not told of the registration")
	}
	if names := r.Snapshot().Names(); len(names) != 1 || names[0] != "one" {
		t.Errorf("snapshot = %v", names)
	}
	// A tool without a name is refused, and nothing changes.
	before := r.Version()
	r.Register(newMockTool("", "nameless"))
	if r.Version() != before {
		t.Error("a refused registration changed the registry")
	}
	if _, ok := r.Get(""); ok {
		t.Error("a tool without a name was registered")
	}

	// A hidden suite tool registers hidden and suite-exempt.
	r.RegisterEntries(suiteEntry(hiddenEntry(newMockTool("hidden_suite", "h"), "g", false)))
	if i, ok := r.cat.Get("hidden_suite"); !ok || !i.Hidden || !isSuiteExempt(i) {
		t.Errorf("hidden suite tool: %+v %v", i, ok)
	}
}

// The discovery meta-tools the skip function names are left out.
func TestRegisterDiscoveryTools_Skip(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterDiscoveryTools(context.Background(), 5, func(name string) bool { return name == "get_tool_details" })
	if _, ok := r.Get("search_tools"); !ok {
		t.Error("search_tools not registered")
	}
	if _, ok := r.Get("get_tool_details"); ok {
		t.Error("a skipped meta-tool was registered")
	}
}

// --- MCP tool names and results ---

func TestMCPTool_NamesStayDistinct(t *testing.T) {
	name := func(server, tool string) string {
		return NewMCPTool(&MockMCPManager{}, server, mcp.Tool{Name: tool}).Name()
	}
	// Lossy sanitization adds a hash of the original names, so two names
	// that differ only in disallowed characters stay distinct.
	a, b := name("Files", "read file"), name("Files", "read.file")
	if a == b || !strings.HasPrefix(a, "mcp_files_read_file_") || len(a) != len("mcp_files_read_file_")+8 {
		t.Errorf("lossy names: %q %q", a, b)
	}
	// Long names are cut to 64 characters, still distinct.
	long1, long2 := name("server", strings.Repeat("x", 80)+"1"), name("server", strings.Repeat("x", 80)+"2")
	if len(long1) > 64 || len(long2) > 64 || long1 == long2 {
		t.Errorf("long names: %q %q", long1, long2)
	}
	// Nothing usable left: "unnamed".
	if got := name("@@@", "tool"); !strings.HasPrefix(got, "mcp_unnamed_tool_") {
		t.Errorf("empty component: %q", got)
	}
	tool := NewMCPTool(&MockMCPManager{}, "fusion", mcp.Tool{Name: "trello_search"})
	if tool.ExternalName() != "fusion_trello_search" {
		t.Errorf("ExternalName = %q", tool.ExternalName())
	}
	if MCPServerPrefix("My Server") != "mcp_my_server_" || MCPServerPattern("My Server") != "mcp_my_server_*" {
		t.Errorf("prefix %q pattern %q", MCPServerPrefix("My Server"), MCPServerPattern("My Server"))
	}
}

func TestMCPTool_SchemaAndResults(t *testing.T) {
	bad := NewMCPTool(&MockMCPManager{}, "s", mcp.Tool{Name: "t", RawInputSchema: []byte("{not json")})
	if p := bad.Parameters(); p["type"] != "object" || bad.HasInputSchema() {
		t.Errorf("unreadable raw schema: %v %v", p, bad.HasInputSchema())
	}
	good := NewMCPTool(&MockMCPManager{}, "s", mcp.Tool{Name: "t", RawInputSchema: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)})
	if !good.HasInputSchema() {
		t.Error("raw schema not seen")
	}
	structured := NewMCPTool(&MockMCPManager{}, "s", mcp.Tool{Name: "t", InputSchema: mcp.ToolInputSchema{Type: "object"}})
	none := NewMCPTool(&MockMCPManager{}, "s", mcp.Tool{Name: "t"})
	if !structured.HasInputSchema() || none.HasInputSchema() {
		t.Error("structured schema")
	}

	nilResult := NewMCPTool(&MockMCPManager{callToolFunc: func(context.Context, string, string, map[string]any) (*mcp.CallToolResult, error) {
		return nil, nil
	}}, "s", mcp.Tool{Name: "t"})
	if res := nilResult.Execute(context.Background(), nil); !res.IsError || res.ForLLM != "MCP tool execution failed: nil result" {
		t.Errorf("nil result: %+v", res)
	}

	text, images := extractContent([]mcp.Content{mcp.AudioContent{Type: "audio", MIMEType: "audio/wav"}})
	if text != "[Content: mcp.AudioContent]" || images != nil {
		t.Errorf("other content: %q %v", text, images)
	}
}

// --- log redaction ---

// Range edits and shell input log only sizes, never the text.
func TestRedactArgs_NeverLogsText(t *testing.T) {
	const secret = "alice's password"
	for tool, args := range map[string]map[string]any{
		"file_edit_lines":   {"path": "f", "start": 1, "end": 2, "replace": secret},
		"file_insert_bytes": {"path": "f", "at_offset": 3, "text": secret},
		"file_delete_lines": {"path": "f", "start": 1},
		"file_write":        {"path": "f", "content": secret, "overwrite": true},
		"shell_ctrl":        {"action": "input", "id": "j1", "text": secret},
		"http_request":      {"url": "https://example.com", "method": "POST", "body": secret},
	} {
		got := redactArgs(tool, args)
		m, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("%s: %T", tool, got)
		}
		for k, v := range m {
			if s, ok := v.(string); ok && strings.Contains(s, "password") {
				t.Errorf("%s: %s holds the text", tool, k)
			}
		}
		switch tool {
		case "file_edit_lines":
			if m["replace_bytes"] != len(secret) || m["start"] != 1 || m["end"] != 2 {
				t.Errorf("%s: %v", tool, m)
			}
		case "file_insert_bytes", "shell_ctrl":
			if m["text_bytes"] != len(secret) {
				t.Errorf("%s: %v", tool, m)
			}
		case "file_write":
			if m["overwrite"] != true || m["content_bytes"] != len(secret) {
				t.Errorf("%s: %v", tool, m)
			}
		case "http_request":
			if m["method"] != "POST" || len(m) != 2 {
				t.Errorf("%s: %v", tool, m)
			}
		}
	}
	if m := redactArgs("file_write", map[string]any{"path": "f", "content": 42}).(map[string]any); m["content_bytes"] != 0 {
		t.Errorf("non-string content: %v", m)
	}
}

func TestTruncateForRedaction_Edges(t *testing.T) {
	if got := truncateForRedaction(map[string]any{"c": make(chan int)}, 200); !strings.HasPrefix(got, "<unmarshalable args: ") {
		t.Errorf("unmarshalable: %q", got)
	}
	if got := truncateForRedaction(map[string]any{"a": "b"}, 0); got != "" {
		t.Errorf("zero length: %q", got)
	}
}

// --- agent messages ---

func TestAgentMessageLimit(t *testing.T) {
	if AgentMessageTooLong(strings.Repeat("é", MaxAgentMessageChars)) || !AgentMessageTooLong(strings.Repeat("a", MaxAgentMessageChars+1)) {
		t.Error("the limit counts characters")
	}
	if got := AgentMessageLimitText(); got != "Messages to other agents are limited to 8,000 characters." {
		t.Errorf("limit text = %q", got)
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := groupThousands(n); got != want {
			t.Errorf("groupThousands(%d) = %q", n, got)
		}
	}
}

// --- turn scope ---

func TestTurnChat_Problems(t *testing.T) {
	bg := context.Background()
	for _, tc := range []struct {
		ctx  context.Context
		want ChatProblem
	}{
		{bg, ChatNone},
		{WithReplyChat(bg, ReplyChat{Channel: "telegram", ChatID: "1", Ambiguous: true}), ChatAmbiguous},
		{WithReplyChat(bg, ReplyChat{Channel: constants.AgentMessageChannel, ChatID: "1"}), ChatAgent},
		{WithReplyChat(bg, ReplyChat{Channel: "cli", ChatID: "1"}), ChatInternal},
		{WithReplyChat(bg, ReplyChat{Channel: "telegram"}), ChatInternal},
		{WithReplyChat(bg, ReplyChat{Channel: "telegram", ChatID: "1"}), ChatOK},
	} {
		if _, got := TurnChat(tc.ctx); got != tc.want {
			t.Errorf("TurnChat = %v, want %v", got, tc.want)
		}
	}
	if ch, id := (ReplyChat{Channel: "telegram", ChatID: "1", Ambiguous: true}).Target(); ch != "" || id != "" {
		t.Error("an ambiguous chat has no target")
	}
	if ch, id := (ReplyChat{Channel: "telegram", ChatID: "1"}).Target(); ch != "telegram" || id != "1" {
		t.Error("target")
	}
}

func TestAskChain(t *testing.T) {
	ctx := context.Background()
	if AskChain(ctx) != nil {
		t.Error("no chain")
	}
	ctx = WithAskChainAgent(ctx, "alice")
	ctx = WithAskChainAgent(ctx, "bob")
	ctx = WithAskChainAgent(ctx, "alice") // once
	ctx = WithAskChainAgent(ctx, "")      // nothing
	if got := AskChain(ctx); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("chain = %v", got)
	}
}

func TestUnattendedAndRoundFlag(t *testing.T) {
	ctx := context.Background()
	if IsUnattended(ctx) || !IsUnattended(withUnattended(ctx)) {
		t.Error("unattended")
	}
	if RoundSentFlagFromCtx(ctx) != nil {
		t.Error("no flag")
	}
	var flag atomic.Bool
	if RoundSentFlagFromCtx(WithRoundSentFlag(ctx, &flag)) != &flag {
		t.Error("flag")
	}
}

// --- temporary agent options ---

func TestFreshAndCloneOptions(t *testing.T) {
	o := NewFreshOptions(WithName("Alice"), WithSystemPrompt(""), WithoutMemory(), SingleShot(), WithCharacterCard("  card-1 "))
	if o.Name != "Alice" || !o.SystemPromptSet || o.SystemPrompt != "" || !o.NoMemory || !o.SingleShot || o.CharacterCard != "card-1" {
		t.Errorf("fresh options: %+v", o)
	}
	if c := NewCloneOptions(WithCharacterCard("card-2")); c.CharacterCard != "card-2" {
		t.Errorf("clone options: %+v", c)
	}
	if o := NewFreshOptions(); o != (FreshOptions{}) {
		t.Errorf("no options: %+v", o)
	}
}

// --- providers ---

// plainSpecProvider is a toolspec provider without host metadata.
type plainSpecProvider struct{}

func (plainSpecProvider) RegisterTools(toolspec.Deps) []toolspec.ToolDefinition {
	return []toolspec.ToolDefinition{{Name: "fusion_thing", Description: "d", Handler: func(*toolspec.ToolCall) (*toolspec.Result, error) {
		return &toolspec.Result{ForLLM: "ok"}, nil
	}}}
}

// unavailableProvider has host metadata and says it is not available.
type unavailableProvider struct{ plainSpecProvider }

func (unavailableProvider) Namespace() string            { return "off" }
func (unavailableProvider) Description() string          { return "Switched off" }
func (unavailableProvider) Available(any) (bool, string) { return false, "not configured" }

func TestNamespacedProvider_Metadata(t *testing.T) {
	bare := BareNamespacedProvider("fusion", plainSpecProvider{})
	tools := bare.Build(ToolDeps{})
	if len(tools) != 1 || tools[0].Name() != "fusion_thing" {
		t.Errorf("bare names: %v", tools)
	}
	if bare.Namespace() != "fusion" || bare.Description() != "fusion" || bare.Category() != "fusion" || bare.ConfigKey() != "fusion" {
		t.Errorf("metadata without HostMeta: %q %q", bare.Namespace(), bare.Description())
	}
	if ok, why := bare.Available(&config.Config{}); !ok || why != "" {
		t.Errorf("available without HostMeta: %v %q", ok, why)
	}
	prefixed := NamespacedProvider("off", unavailableProvider{})
	if prefixed.Description() != "Switched off" {
		t.Errorf("description = %q", prefixed.Description())
	}
	if ok, why := prefixed.Available(&config.Config{}); ok || why != "not configured" {
		t.Errorf("available = %v %q", ok, why)
	}
	if got := prefixed.Build(ToolDeps{}); len(got) != 1 || got[0].Name() != "off_fusion_thing" {
		t.Errorf("prefixed names: %v", got)
	}
}

func TestRegisterProvider_IdempotentAndUnregister(t *testing.T) {
	p := defaultEnabledTestProvider{descs: []ToolDescriptor{{Name: "ztest_on", DefaultEnabled: true}}}
	RegisterProvider(p)
	RegisterProvider(p)
	t.Cleanup(func() { UnregisterProvider(p.Namespace()) })
	count := 0
	for _, q := range GetProviders() {
		if q.Namespace() == p.Namespace() {
			count++
		}
	}
	if count != 1 {
		t.Errorf("registered %d times", count)
	}

	saved := StaticToolDescriptors
	StaticToolDescriptors = []ToolDescriptor{{Name: "static_on", DefaultEnabled: true}, {Name: "static_off"}}
	t.Cleanup(func() { StaticToolDescriptors = saved })
	names := DefaultEnabledToolNames()
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("ztest_on") || !has("static_on") || has("static_off") {
		t.Errorf("default tools = %v", names)
	}

	UnregisterProvider(p.Namespace())
	for _, q := range GetProviders() {
		if q.Namespace() == p.Namespace() {
			t.Error("provider not removed")
		}
	}
	UnregisterProvider("never-registered")
}

func TestEffectiveStateDir(t *testing.T) {
	if (ToolDeps{Workspace: "/w"}).EffectiveStateDir() != "/w" || (ToolDeps{Workspace: "/w", StateDir: "/s"}).EffectiveStateDir() != "/s" {
		t.Error("EffectiveStateDir")
	}
}
