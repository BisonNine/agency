// ClawEh
// License: MIT

package agent

import (
	"errors"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// reasoningPlaceholder is what the SDK engine writes into an empty
// reasoning_content: a single space, since DeepSeek V4 Pro rejects "" and
// anything longer would be fabricated reasoning. It must equal the SDK's.
const reasoningPlaceholder = " "

// Two endpoints: one that demands reasoning_content (DeepSeek V4 thinking
// mode) and one that does not.
var (
	backfillProviders = []config.Provider{
		{Name: "ds", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k", RequireReasoningContent: true},
		{Name: "plain", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"},
	}
	dsPro      = config.ModelConfig{ModelName: "ds-pro", Provider: "ds"}
	plainModel = config.ModelConfig{ModelName: "other", Provider: "plain"}
)

// mixedHistory is what a session looks like after switching models: some
// assistant turns carry real reasoning, some (written by a CLI model, or by the
// same model with thinking off) carry none.
func mixedHistory() []providers.Message {
	return []providers.Message{
		{Role: "user", Content: "write chapter 3"},
		{Role: "assistant", Content: "drafting", ReasoningContent: "real reasoning"},
		{Role: "user", Content: "go on"},
		{Role: "assistant", Content: "calling a tool", ToolCalls: []providers.ToolCall{toolCallTo("t1", "echo_tool")}},
		{Role: "tool", ToolCallID: "t1", Content: "result"},
		{Role: "assistant", Content: "written by a CLI model, no reasoning"},
	}
}

// reasoningOf is the reasoning_content of the message of msgs with content.
func reasoningOf(t *testing.T, msgs []providers.Message, content string) string {
	t.Helper()
	return messageWith(t, msgs, content).ReasoningContent
}

// TestBackfill_FillsEmptyAssistantReasoning is the case this exists for: an
// agent switched from a CLI model to DeepSeek replays a history with no
// reasoning_content, and DeepSeek rejects it with a 400 on every turn.
func TestBackfill_FillsEmptyAssistantReasoning(t *testing.T) {
	requests, _ := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{dsPro}, mixedHistory(), nil)
	req := requests[0].messages
	for i, m := range req {
		if m.Role == "assistant" && m.ReasoningContent == "" {
			t.Errorf("message %d: assistant turn still has empty reasoning_content", i)
		}
	}
	// Real reasoning must survive untouched — this is a backfill, not a rewrite.
	if got := reasoningOf(t, req, "drafting"); got != "real reasoning" {
		t.Errorf("existing reasoning was overwritten: %q", got)
	}
	// The placeholder must be non-empty; DeepSeek V4 Pro rejects "".
	if got := reasoningOf(t, req, "calling a tool"); got != reasoningPlaceholder || got == "" {
		t.Errorf("tool-call turn: got %q, want the non-empty placeholder", got)
	}
	if got := reasoningOf(t, req, "written by a CLI model"); got != reasoningPlaceholder {
		t.Errorf("plain assistant turn: got %q, want the placeholder", got)
	}
}

// TestBackfill_OnlyAssistantMessages keeps the placeholder off user and tool
// turns, which have no such field in the wire contract.
func TestBackfill_OnlyAssistantMessages(t *testing.T) {
	requests, _ := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{dsPro}, mixedHistory(), nil)
	for i, m := range requests[0].messages {
		if m.Role != "assistant" && m.ReasoningContent != "" {
			t.Errorf("message %d (%s): reasoning_content set on a non-assistant turn", i, m.Role)
		}
	}
}

// TestBackfill_RequiresTools covers the conditional half of DeepSeek's contract:
// without tools in the request the field is ignored entirely, so there is
// nothing to satisfy and nothing to add.
func TestBackfill_RequiresTools(t *testing.T) {
	toolless := dsPro
	toolless.NoTools = true
	requests, _ := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{toolless}, mixedHistory(), nil)
	if got := reasoningOf(t, requests[0].messages, "written by a CLI model"); got != "" {
		t.Errorf("no tools in the request: expected no backfill, got %q", got)
	}
}

// TestBackfill_OnlyWhenProviderOptsIn is the off path — an endpoint that never
// asked for this must see history exactly as stored.
func TestBackfill_OnlyWhenProviderOptsIn(t *testing.T) {
	requests, _ := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{plainModel}, mixedHistory(), nil)
	if got := reasoningOf(t, requests[0].messages, "written by a CLI model"); got != "" {
		t.Errorf("provider did not opt in: expected no backfill, got %q", got)
	}
}

// TestBackfill_DoesNotMutateInput is load-bearing for the fallback chain: the
// same history is handed to each candidate in turn, so one candidate's wire
// quirk must not follow it to the next, nor reach the stored history.
func TestBackfill_DoesNotMutateInput(t *testing.T) {
	fail := map[string]error{dsPro.ModelName: errors.New(rateLimited)}
	requests, stored := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{dsPro, plainModel}, mixedHistory(), fail)
	if len(requests) != 2 || requests[0].model != dsPro.ModelName || requests[1].model != plainModel.ModelName {
		t.Fatalf("requests went to %+v, want %s then %s", requests, dsPro.ModelName, plainModel.ModelName)
	}
	if got := reasoningOf(t, requests[0].messages, "written by a CLI model"); got != reasoningPlaceholder {
		t.Errorf("first candidate: got %q, want the placeholder", got)
	}
	// The next candidate, on a provider that rejects the field, sees it clean.
	if got := reasoningOf(t, requests[1].messages, "written by a CLI model"); got != "" {
		t.Errorf("placeholder leaked to a provider that did not ask for it: %q", got)
	}
	if got := reasoningOf(t, stored, "written by a CLI model"); got != "" {
		t.Errorf("stored history was mutated: %q", got)
	}
}

// TestBackfill_NothingToFill: once a thinking model has been running, every
// assistant turn already has reasoning and the request carries it as stored.
func TestBackfill_NothingToFill(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "there", ReasoningContent: "thought"},
	}
	requests, _ := storedHistoryTurn(t, backfillProviders, []config.ModelConfig{dsPro}, history, nil)
	if got := reasoningOf(t, requests[0].messages, "there"); got != "thought" {
		t.Errorf("reasoning = %q, want it as stored", got)
	}
}
