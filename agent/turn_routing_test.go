// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// Memory recalls for the message with its whispers, before its inbound media
// are described and their references appended: the turn's RecallText is what
// the host has always recalled for, while the stored message carries the
// media's references.
func TestTurn_RecallTextHasWhispersNotMedia(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	agent := tl.al.registry.Default()
	tl.al.whispers.add(agent.ID, whisper{from: "Alice", note: "a person", text: "psst"})
	var recall, text string
	probe := hook.Plugin{Name: "recall-probe", BeforeTurn: func(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
		recall, text = turn.RecallText, turn.Message.Text
		return nil, nil
	}}
	opts := processOptions{
		SessionKey: "agent:main:recall", Channel: "telegram", ChatID: "alice",
		UserMessage: "hello", Media: []string{"media://11111111-1111-1111-1111-111111111111"},
	}
	tm := tl.al.selectTurnModel(agent, opts.SessionKey)
	if _, _, _, err := runTestTurn(t, context.Background(), tl.al, agent, opts, tm, nil, []hook.Plugin{probe}, nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !strings.Contains(recall, "psst") || !strings.HasSuffix(recall, "hello") {
		t.Fatalf("recall text = %q, want the whispers and the message", recall)
	}
	if strings.Contains(recall, "media://") {
		t.Fatalf("recall text = %q, carries the media's references", recall)
	}
	if !strings.HasPrefix(text, recall) || !strings.Contains(text, "media://11111111-1111-1111-1111-111111111111") {
		t.Fatalf("message = %q, want the recall text and the media's reference", text)
	}
}

// gatedAsyncTool completes in the background: each call hands its callback
// to the test, which reports the result when it chooses.
type gatedAsyncTool struct{ cbs chan tools.AsyncCallback }

func (gatedAsyncTool) Name() string               { return "gated_async" }
func (gatedAsyncTool) Description() string        { return "completes when the test says" }
func (gatedAsyncTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (gatedAsyncTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return &tools.ToolResult{ForLLM: "sync"}
}

func (g gatedAsyncTool) ExecuteAsync(ctx context.Context, _ map[string]any, cb tools.AsyncCallback) *tools.ToolResult {
	g.cbs <- cb
	return &tools.ToolResult{ForLLM: "started", Async: true}
}

// hookedProvider answers each call with next(n), n counting from 0.
type hookedProvider struct {
	mu   sync.Mutex
	n    int
	next func(n int) *spawnllm.LLMResponse
}

func (p *hookedProvider) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	n := p.n
	p.n++
	p.mu.Unlock()
	return p.next(n), nil
}

func (p *hookedProvider) GetDefaultModel() string { return "test-model" }

// A background tool's late result goes to the chat its call was made for:
// read from the context the call ran on, both when it arrives after the turn
// ended and when it arrives while another turn of the session, for another
// chat, runs.
func TestAsyncResult_GoesToTheCallsChat(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	cfg.Agents.List[0].Tools = []string{"*"}
	secondTurn := make(chan struct{})
	release := make(chan struct{})
	p := &hookedProvider{next: func(n int) *spawnllm.LLMResponse {
		switch n {
		case 0:
			return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{toolCallTo("tc-1", "gated_async"), toolCallTo("tc-2", "gated_async")}}
		case 1:
			return &spawnllm.LLMResponse{Content: "started both"}
		default:
			close(secondTurn)
			<-release
			return &spawnllm.LLMResponse{Content: "second"}
		}
	}}
	msgBus := bus.NewMessageBus()
	al := mustNewAgentLoop(t, cfg, msgBus, p, nil)
	tool := gatedAsyncTool{cbs: make(chan tools.AsyncCallback, 2)}
	al.RegisterTool(tool)
	agent := al.GetRegistry().Default()
	const session = "agent:main:shared"
	first := processOptions{SessionKey: session, Channel: "telegram", ChatID: "c1", UserMessage: "start"}
	if _, err := al.runAgentLoop(context.Background(), agent, first); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	cb1, cb2 := <-tool.cbs, <-tool.cbs

	inboundChat := func() (string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		in, ok := msgBus.ConsumeInbound(ctx)
		if !ok || in.Channel != "system" {
			t.Fatalf("no re-injected system message (%+v)", in)
		}
		return in.ChatID, in.Metadata[bus.MetaOriginChannel]
	}

	// After the turn ended.
	cb1(context.Background(), &tools.ToolResult{ForLLM: "late one", Silent: true})
	if chat, origin := inboundChat(); chat != "c1" || origin != "telegram" {
		t.Fatalf("late result went to %s:%s, want telegram:c1", origin, chat)
	}

	// While another turn of the session, for another chat, runs.
	done := make(chan error, 1)
	go func() {
		_, err := al.runAgentLoop(context.Background(), agent, processOptions{SessionKey: session, Channel: "discord", ChatID: "c2", UserMessage: "again"})
		done <- err
	}()
	<-secondTurn
	cb2(context.Background(), &tools.ToolResult{ForLLM: "late two", Silent: true})
	chat, origin := inboundChat()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if chat != "c1" || origin != "telegram" {
		t.Fatalf("result during another turn went to %s:%s, want telegram:c1", origin, chat)
	}
}

// An agent with no usable model (an empty chain) fails its turn with the
// host's "no model configured" text and makes no call; a chain whose every
// model became unavailable during the turn fails with the chain's own error,
// which is not that.
func TestTurn_NoModelAndUnavailableChain(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	_, _, err := testTurn(t, nil, &AgentInstance{ID: "bob", MaxIterations: 1}, processOptions{}, &turnModel{}, nil)
	if !errors.Is(err, errNoModelConfigured) {
		t.Fatalf("no model: err = %v, want errNoModelConfigured", err)
	}
	if want := "LLM call failed after retries: " + errNoModelConfigured.Error(); err.Error() != want {
		t.Fatalf("no model: text = %q, want %q", err.Error(), want)
	}

	run := chainTurn(t, modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
		return answerOK(model), nil
	}}, nil, candAlice)
	if derr := run.al.catalog.Disable(candAlice.Name); derr != nil {
		t.Fatal(derr)
	}
	_, err = run.run(context.Background())
	if !errors.Is(err, hook.ErrNoModelAvailable) || errors.Is(err, errNoModelConfigured) {
		t.Fatalf("unavailable chain: err = %v, want the chain's own no-model error", err)
	}
	if !strings.Contains(err.Error(), "every model is disabled") {
		t.Fatalf("unavailable chain: text = %q", err.Error())
	}
}
