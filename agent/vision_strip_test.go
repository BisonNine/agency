package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
)

// modelRequest is one request a model was sent.
type modelRequest struct {
	model    string
	messages []spawnllm.Message
	tools    int
}

// requestRecorder records every request and answers it with a plain reply,
// or with fail's error for the wire model.
type requestRecorder struct {
	mu       sync.Mutex
	requests []modelRequest
	fail     map[string]error
}

func (p *requestRecorder) Chat(_ context.Context, _ accounting.Identity, messages []spawnllm.Message, tools []spawnllm.ToolDefinition, model string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, modelRequest{model: model, messages: slices.Clone(messages), tools: len(tools)})
	if err := p.fail[model]; err != nil {
		return nil, err
	}
	return &spawnllm.LLMResponse{Content: "ok", FinishReason: "stop", Normal: true}, nil
}

func (*requestRecorder) GetDefaultModel() string { return "test-model" }

// storedHistoryTurn runs one whole turn ("look") of the default agent of a
// loop whose chain is mcs, in order (each model's wire name is its name), on
// the test provider plus extra, with history already in its session. A model
// configured with no_tools is offered none; fail makes a wire model answer
// with an error.
// It returns the requests the models were sent and the stored history after
// the turn.
func storedHistoryTurn(t *testing.T, extra []modelspec.Provider, mcs []modelspec.Model, history []spawnllm.Message, fail map[string]error) ([]modelRequest, []spawnllm.Message) {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.Providers = append(cfg.Providers, extra...)
	names := make([]string, 0, len(mcs))
	noTools := map[string]bool{}
	for _, mc := range mcs {
		noTools[mc.ModelName] = mc.NoTools
		if mc.Provider == "" {
			mc.Provider = testProviderName
		}
		mc.Model, mc.Enabled = mc.ModelName, true
		cfg.Models = append(cfg.Models, mc)
		names = append(names, mc.ModelName)
	}
	cfg.Agents.Defaults.Models = names
	cfg.Agents.List[0].Tools = []string{"*"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	p := &requestRecorder{fail: fail}
	setTestProvider(agent, p)
	key := "agent:" + agent.ID + ":main"
	for _, m := range history {
		if _, err := agent.Sessions.AddFullMessage(key, m); err != nil {
			t.Fatal(err)
		}
	}
	opts := processOptions{SessionKey: key, UserMessage: "look"}
	if _, _, err := testTurn(t, al, agent, opts, al.selectTurnModel(agent, key), nil); err != nil {
		t.Fatalf("turn: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("no model was called")
	}
	for i, r := range p.requests {
		if (r.tools == 0) != noTools[r.model] {
			t.Fatalf("request %d offered %d tools, want tools offered: %v", i+1, r.tools, !noTools[r.model])
		}
	}
	return p.requests, agent.Sessions.GetHistory(key)
}

// imageToolResult is the tool result carrying an image in historyWithImage.
const imageToolResult = "[Image: image/png]"

// historyWithImage is a session whose tool result carries an image.
func historyWithImage() []spawnllm.Message {
	return []spawnllm.Message{
		{Role: "user", Content: "check tracking"},
		{Role: "assistant", ToolCalls: []spawnllm.ToolCall{toolCallTo("t1", "echo_tool")}},
		{Role: "tool", ToolCallID: "t1", Content: imageToolResult, Media: []string{"data:image/png;base64,AAA"}},
		{Role: "assistant", Content: "here it is"},
	}
}

// messageWith is the message of msgs whose content starts with prefix.
func messageWith(t *testing.T, msgs []spawnllm.Message, prefix string) spawnllm.Message {
	t.Helper()
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, prefix) {
			return m
		}
	}
	t.Fatalf("no message starts with %q in %+v", prefix, msgs)
	return spawnllm.Message{}
}

// A vision-capable model must receive the image untouched.
func TestMessagesForModel_VisionModelKeepsImages(t *testing.T) {
	for _, vision := range []string{modelspec.VisionUserMessage, modelspec.VisionToolResponse} {
		requests, _ := storedHistoryTurn(t, nil, []modelspec.Model{{ModelName: "gpt5", Vision: vision}}, historyWithImage(), nil)
		if got := messageWith(t, requests[0].messages, imageToolResult); len(got.Media) != 1 || got.Content != imageToolResult {
			t.Errorf("%s: expected the image kept untouched, got %q, Media=%v", vision, got.Content, got.Media)
		}
	}
}

// A non-vision model (off or unset) must not receive image parts — this is
// the fix for OpenRouter 404ing the whole request on a non-vision default
// like deepseek. The persisted history must be left intact.
func TestMessagesForModel_NonVisionStripsImagesWithoutMutating(t *testing.T) {
	for _, vision := range []string{modelspec.VisionOff, ""} {
		requests, stored := storedHistoryTurn(t, nil, []modelspec.Model{{ModelName: "deepseek", Vision: vision}}, historyWithImage(), nil)
		got := messageWith(t, requests[0].messages, imageToolResult)
		if got.Media != nil {
			t.Errorf("%q: expected images stripped, got Media=%v", vision, got.Media)
		}
		// Text (incl. the [Image: …] marker) is preserved and a hidden-attachment
		// note is appended so the model knows something was stripped.
		if !strings.Contains(got.Content, "1 attachment(s) on this message are hidden") {
			t.Errorf("%q: expected hidden-attachment note, got %q", vision, got.Content)
		}
		// The stored history must not be mutated: a later vision turn still sees it.
		if m := messageWith(t, stored, imageToolResult); len(m.Media) != 1 || m.Content != imageToolResult {
			t.Errorf("%q: stored history mutated: %q, Media=%v", vision, m.Content, m.Media)
		}
	}
}

// When there are no images, a non-vision model's request carries the
// history unchanged.
func TestMessagesForModel_NoImagesLeavesRequestUnchanged(t *testing.T) {
	history := []spawnllm.Message{
		{Role: "user", Content: "no images here"},
		{Role: "assistant", Content: "noted"},
	}
	requests, _ := storedHistoryTurn(t, nil, []modelspec.Model{{ModelName: "deepseek", Vision: modelspec.VisionOff}}, history, nil)
	for _, want := range history {
		if got := messageWith(t, requests[0].messages, want.Content); got.Role != want.Role || got.Content != want.Content || got.Media != nil {
			t.Errorf("request carries %s %q, want %s %q unchanged", got.Role, got.Content, want.Role, want.Content)
		}
	}
	for _, m := range requests[0].messages {
		if strings.Contains(m.Content, "are hidden") {
			t.Errorf("request carries a hidden-attachment note: %q", m.Content)
		}
	}
}
