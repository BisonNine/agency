// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdkagent "go.bison9.com/agent"
	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// A turn's tool calls run through the SDK agent's gate: the host's allow
// gate on the exact tool the call resolved to, on a context carrying the
// turn's chat, agent id, allow checker and reply chat.

// gateModel calls the tool named call on a person's message, records the
// result it gets back and replies "done: <result>". Any other message (a
// background result re-entering) is answered "ok".
type gateModel struct {
	call string

	mu      sync.Mutex
	results []string
}

func (m *gateModel) GetDefaultModel() string { return "mock-model" }

func (m *gateModel) Chat(_ context.Context, _ accounting.Identity, messages []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	last := messages[len(messages)-1]
	switch {
	case last.Role == "tool":
		m.mu.Lock()
		m.results = append(m.results, last.Content)
		m.mu.Unlock()
		return &spawnllm.LLMResponse{Content: "done: " + last.Content}, nil
	case strings.Contains(last.Content, "run it"):
		return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
			ID: "tc-1", Type: "function", Name: m.call,
			Function: &spawnllm.FunctionCall{Name: m.call, Arguments: "{}"},
		}}}, nil
	default:
		return &spawnllm.LLMResponse{Content: "ok"}, nil
	}
}

// take returns the tool results recorded so far.
func (m *gateModel) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.results...)
}

// gateTool is a tool registered under an internal name and offered under
// an external one. It records the call context it ran on; as an async tool
// it reports late once release is closed.
type gateTool struct {
	name, external string
	release        chan struct{}

	mu   sync.Mutex
	seen context.Context
}

func (g *gateTool) Name() string               { return g.name }
func (g *gateTool) ExternalName() string       { return g.external }
func (g *gateTool) Description() string        { return "a probe" }
func (g *gateTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (g *gateTool) Execute(ctx context.Context, _ map[string]any) *toolkit.Result {
	g.mu.Lock()
	g.seen = ctx
	g.mu.Unlock()
	return toolkit.SilentResult("probe ran")
}

// gateAsyncTool is gateTool as an async tool.
type gateAsyncTool struct{ gateTool }

func (g *gateAsyncTool) ExecuteAsync(ctx context.Context, _ map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	go func() {
		<-g.release
		cb(ctx, &toolkit.Result{ForUser: "late for you", ForLLM: "late for the model"})
	}()
	return toolkit.AsyncResult("started")
}

// gateContext is the context the tool last ran on.
func (g *gateTool) gateContext() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen
}

// gateLoop is messagingLoop over messagingConfig with model, Alice granted
// the MCP tools "alice_*", and a captured log.
func gateLoop(t *testing.T, model *gateModel) (*AgentLoop, *bus.MessageBus, *safeBufLoop) {
	t.Helper()
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	cfg := messagingConfig(t)
	cfg.Agents.List[0].MCPTools = []string{"alice"}
	al, msgBus := messagingLoop(t, cfg, model)
	return al, msgBus, logs
}

// instanceOf is the loop's instance for id.
func instanceOf(t *testing.T, al *AgentLoop, id string) *AgentInstance {
	t.Helper()
	inst, ok := al.GetRegistry().Get(id)
	if !ok {
		t.Fatalf("no agent %q", id)
	}
	return inst
}

// runIt sends "run it" to agent id in its own Telegram chat and returns the
// reply.
func runIt(t *testing.T, al *AgentLoop, msgBus *bus.MessageBus, id string) bus.OutboundMessage {
	t.Helper()
	msg := inbound("chat-"+id, "m-"+id, "run it")
	msg.Channel = "telegram"
	msg.Metadata = map[string]string{metadataKeyPreresolvedAgentID: id}
	dispatch(al, msg)
	return nextOutboundTo(t, msgBus, "chat-"+id)
}

// A call of a tool the agent is not granted, made by its external name, is
// refused by the host's gate with the registry's own refusal (naming the
// tool as the model called it) and one warning naming the internal tool.
func TestToolGate_RefusedCallThroughATurn(t *testing.T) {
	model := &gateModel{call: "bob_probe"}
	al, msgBus, logs := gateLoop(t, model)
	bob := instanceOf(t, al, "bob")
	probe := &gateTool{name: "mcp_bob_probe", external: "bob_probe"}
	bob.Tools.Register(probe)

	runIt(t, al, msgBus, "bob")

	want := toolkit.NotEnabledMessage("bob_probe")
	if got := model.take(); len(got) != 1 || got[0] != want {
		t.Fatalf("tool results = %q, want exactly %q", got, want)
	}
	if probe.gateContext() != nil {
		t.Fatal("the refused tool ran")
	}
	var denied []map[string]any
	for l := range strings.SplitSeq(logs.String(), "\n") {
		if !strings.Contains(l, "Tool execution denied by agent allowlist") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		denied = append(denied, rec)
	}
	if len(denied) != 1 || denied[0]["level"] != "warn" || denied[0]["tool"] != "mcp_bob_probe" {
		t.Fatalf("denial lines = %v, want one warning for mcp_bob_probe", denied)
	}

	// The registry's own execution refuses the call with the same text.
	ctx := tools.WithToolAllowChecker(context.Background(), bob.Config)
	if r := bob.Tools.ExecuteWithContext(ctx, "bob_probe", nil, "", "", nil); r.ForLLM != want {
		t.Errorf("registry refusal = %q, want %q", r.ForLLM, want)
	}
}

// A granted tool runs on a context carrying the turn's chat, the agent id,
// the agent's allow checker and the reply chat.
func TestToolGate_AllowedCallContext(t *testing.T) {
	model := &gateModel{call: "alice_probe"}
	al, msgBus, _ := gateLoop(t, model)
	alice := instanceOf(t, al, "alice")
	probe := &gateTool{name: "mcp_alice_probe", external: "alice_probe"}
	alice.Tools.Register(probe)

	if out := runIt(t, al, msgBus, "alice"); out.Content != "done: probe ran" {
		t.Fatalf("reply = %q, want the tool's result", out.Content)
	}
	ctx := probe.gateContext()
	if ctx == nil {
		t.Fatal("the tool did not run")
	}
	if toolkit.Channel(ctx) != "telegram" || toolkit.ChatID(ctx) != "chat-alice" || toolkit.AgentID(ctx) != "alice" {
		t.Errorf("call for %q/%q by %q, want telegram/chat-alice by alice",
			toolkit.Channel(ctx), toolkit.ChatID(ctx), toolkit.AgentID(ctx))
	}
	if tools.ToolAllowCheckerFromCtx(ctx) == nil {
		t.Error("no allow checker on the call")
	}
	if rc, ok := tools.ReplyChatOf(ctx); !ok || rc.Channel != "telegram" || rc.ChatID != "chat-alice" {
		t.Errorf("reply chat = %+v, %v; want telegram/chat-alice", rc, ok)
	}
	if call, ok := hook.ToolCallFrom(ctx); !ok || call.Name != "alice_probe" {
		t.Errorf("tool call on the context = %+v, %v", call, ok)
	}
}

// An async tool's late result reaches the host with the tool the call
// resolved to, and is delivered to the chat as before.
func TestToolGate_AsyncResultInfoResolved(t *testing.T) {
	model := &gateModel{call: "alice_slow"}
	al, msgBus, _ := gateLoop(t, model)
	alice := instanceOf(t, al, "alice")
	slow := &gateAsyncTool{gateTool{name: "mcp_alice_slow", external: "alice_slow", release: make(chan struct{})}}
	alice.Tools.Register(slow)

	// Alice's agent, rebuilt with a sink that records the Info it is given.
	infos := make(chan toolkit.Info, 1)
	a, err := sdkagent.New(alice.ID, al.turnAgentSpec(alice), func(s *loop.Spec) {
		sink := s.AsyncResult
		s.AsyncResult = func(ctx context.Context, call hook.ToolCall, info toolkit.Info, r *toolkit.Result) {
			infos <- info
			sink(ctx, call, info, r)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	alice.turnAgentMu.Lock()
	old := alice.turnAgent
	alice.turnAgent = a
	alice.turnAgentMu.Unlock()
	if old != nil {
		if err := old.Close(context.Background()); err != nil {
			t.Fatalf("close the old agent: %v", err)
		}
	}

	if out := runIt(t, al, msgBus, "alice"); out.Content != "done: started" {
		t.Fatalf("reply = %q, want the started result", out.Content)
	}
	close(slow.release)
	select {
	case info := <-infos:
		if info.Name != "mcp_alice_slow" || info.Published != "alice_slow" || info.Tool != toolkit.Tool(slow) {
			t.Errorf("info = %+v, want the resolved mcp_alice_slow", info)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no late result")
	}
	if out := nextOutboundTo(t, msgBus, "chat-alice"); out.Content != "late for you" {
		t.Errorf("late result = %q, want the tool's ForUser text", out.Content)
	}
}
