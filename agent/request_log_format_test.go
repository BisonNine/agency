// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/utils"
)

// The "Full LLM request" line renders the request's messages and tools the
// same way whether the host-wide no-truncate switch is off (long text cut to
// 200 runes) or on (nothing cut).
func TestFormatRequestForLog_TruncationSwitch(t *testing.T) {
	long := strings.Repeat("é", 250)
	cut := strings.Repeat("é", 197) + "..."
	params := "map[x:" + strings.Repeat("p", 250) + "]"
	messages := []providers.Message{
		{Role: "system", Content: "be brief"},
		{Role: "assistant", Content: long, ToolCalls: []providers.ToolCall{
			{ID: "c1", Type: "function", Name: "file_read", Function: &providers.FunctionCall{Name: "file_read", Arguments: long}},
			{ID: "c2", Type: "function", Name: "noop"},
		}},
		{Role: "tool", Content: "done", ToolCallID: "c1"},
	}
	defs := []providers.ToolDefinition{
		{Type: "function", Function: providers.ToolFunctionDefinition{
			Name: "file_read", Description: long,
			Parameters: map[string]any{"x": strings.Repeat("p", 250)},
		}},
		{Type: "function", Function: providers.ToolFunctionDefinition{Name: "noop", Description: "nothing"}},
	}
	wantMessages := func(text string) string {
		return "[\n" +
			"  [0] Role: system\n  Content: be brief\n\n" +
			"  [1] Role: assistant\n  ToolCalls:\n" +
			"    - ID: c1, Type: function, Name: file_read\n      Arguments: " + text + "\n" +
			"    - ID: c2, Type: function, Name: noop\n" +
			"  Content: " + text + "\n\n" +
			"  [2] Role: tool\n  Content: done\n  ToolCallID: c1\n\n" +
			"]"
	}
	wantTools := func(text, p string) string {
		return "[\n" +
			"  [0] Type: function, Name: file_read\n      Description: " + long + "\n      Parameters: " + p + "\n" +
			"  [1] Type: function, Name: noop\n      Description: nothing\n" +
			"]"
	}
	for _, tc := range []struct {
		name       string
		noTruncate bool
		text, p    string
	}{
		{"truncated", false, cut, params[:197] + "..."},
		{"no-truncate", true, long, params},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.SetDisableTruncation(tc.noTruncate)
			t.Cleanup(func() { utils.SetDisableTruncation(false) })
			if got, want := formatMessagesForLog(messages, utils.Truncate), wantMessages(tc.text); got != want {
				t.Errorf("messages:\n got %q\nwant %q", got, want)
			}
			if got, want := formatToolsForLog(defs, utils.Truncate), wantTools(tc.text, tc.p); got != want {
				t.Errorf("tools:\n got %q\nwant %q", got, want)
			}
			if formatMessagesForLog(nil, utils.Truncate) != "[]" || formatToolsForLog(nil, utils.Truncate) != "[]" {
				t.Error("an empty request is not rendered as []")
			}
		})
	}
}

// The host gives the engine its truncation rule: the "Full LLM request" line
// of a turn follows the no-truncate switch.
func TestFullLLMRequestLine_FollowsTruncationSwitch(t *testing.T) {
	logger.SetLogMessageContent(true)
	t.Cleanup(func() { logger.SetLogMessageContent(false) })
	long := strings.Repeat("b", 250)
	for _, noTruncate := range []bool{false, true} {
		utils.SetDisableTruncation(noTruncate)
		t.Cleanup(func() { utils.SetDisableTruncation(false) })
		var buf safeBufLoop
		restore := logger.RedirectForTest(&buf)
		al := newTestAgentLoop(t).al
		agent := al.registry.Default()
		setTestProvider(agent, &sequenceProvider{responses: []*providers.LLMResponse{{Content: "ok"}}, errors: []error{nil}})
		opts := processOptions{SessionKey: "request-log", Channel: "cli", ChatID: "direct", UserMessage: "go"}
		cm, release := al.getContextManager(agent, opts.SessionKey)
		_, err := al.runLLMIteration(context.Background(), agent, []providers.Message{{Role: "user", Content: long}}, opts, cm, nil)
		release()
		restore()
		if err != nil {
			t.Fatal(err)
		}
		var line map[string]any
		for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(l), &ev) == nil && ev["message"] == "Full LLM request" {
				line = ev
			}
		}
		if line == nil {
			t.Fatalf("no-truncate %v: no \"Full LLM request\" line in %s", noTruncate, buf.String())
		}
		want := strings.Repeat("b", 197) + "..."
		if noTruncate {
			want = long
		}
		if got := fmt.Sprint(line["messages_json"]); !strings.Contains(got, "Content: "+want+"\n") {
			t.Errorf("no-truncate %v: messages_json = %q, want the content as %q", noTruncate, got, want)
		}
	}
}
