// Agency
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/utils"
)

// logTool is a tool with a given name, description and parameters.
type logTool struct {
	name, description string
	parameters        map[string]any
}

func (l logTool) Name() string               { return l.name }
func (l logTool) Description() string        { return l.description }
func (l logTool) Parameters() map[string]any { return l.parameters }
func (logTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return toolkit.SilentResult("done")
}

// fullRequestLine runs a whole turn of the default agent whose first request
// is messages, offering it exactly the given tools (none when empty), and
// returns its "Full LLM request" log line.
func fullRequestLine(t *testing.T, messages []spawnllm.Message, offered ...logTool) map[string]any {
	t.Helper()
	logger.SetLogMessageContent(true)
	t.Cleanup(func() { logger.SetLogMessageContent(false) })
	var buf safeBufLoop
	restore := logger.RedirectForTest(&buf)
	defer restore()
	cfg := newTestConfig(t)
	noCogmem := false
	cfg.Agents.List[0].Cogmem = &noCogmem
	for _, tool := range offered {
		cfg.Agents.List[0].Tools = append(cfg.Agents.List[0].Tools, tool.name)
	}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	for _, tool := range offered {
		agent.Tools.Register(tool)
	}
	setTestProvider(agent, &sequenceProvider{responses: []*spawnllm.LLMResponse{{Content: "ok"}}, errors: []error{nil}})
	opts := processOptions{SessionKey: "request-log", Channel: "cli", ChatID: "direct", UserMessage: "go"}
	cm, release := al.getContextManager(agent, opts.SessionKey)
	defer release()
	if _, err := al.runLLMIteration(context.Background(), agent, messages, opts, cm, nil); err != nil {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(l), &ev) == nil && ev["message"] == "Full LLM request" {
			return ev
		}
	}
	t.Fatalf("no \"Full LLM request\" line in %s", buf.String())
	return nil
}

// The "Full LLM request" line of a turn renders the request's messages and
// tools the same way whether the host-wide no-truncate switch is off (long
// text cut to 200 runes) or on (nothing cut).
func TestFullLLMRequestLine_Format(t *testing.T) {
	long := strings.Repeat("é", 250)
	cut := strings.Repeat("é", 197) + "..."
	params := "map[x:" + strings.Repeat("p", 250) + "]"
	messages := []spawnllm.Message{
		{Role: "system", Content: "be brief"},
		{Role: "assistant", Content: long, ToolCalls: []spawnllm.ToolCall{
			{ID: "c1", Type: "function", Name: "long_read", Function: &spawnllm.FunctionCall{Name: "long_read", Arguments: long}},
			{ID: "c2", Type: "function", Name: "noop"},
		}},
		{Role: "tool", Content: "done", ToolCallID: "c1"},
	}
	offered := []logTool{
		{name: "long_read", description: long, parameters: map[string]any{"x": strings.Repeat("p", 250)}},
		{name: "noop", description: "nothing"},
	}
	wantMessages := func(text string) string {
		return "[\n" +
			"  [0] Role: system\n  Content: be brief\n\n" +
			"  [1] Role: assistant\n  ToolCalls:\n" +
			"    - ID: c1, Type: function, Name: long_read\n      Arguments: " + text + "\n" +
			"    - ID: c2, Type: function, Name: noop\n" +
			"  Content: " + text + "\n\n" +
			"  [2] Role: tool\n  Content: done\n  ToolCallID: c1\n\n" +
			"]"
	}
	wantTools := func(p string) string {
		return "[\n" +
			"  [0] Type: function, Name: long_read\n      Description: " + long + "\n      Parameters: " + p + "\n" +
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
			line := fullRequestLine(t, messages, offered...)
			if got, want := fmt.Sprint(line["messages_json"]), wantMessages(tc.text); got != want {
				t.Errorf("messages:\n got %q\nwant %q", got, want)
			}
			if got, want := fmt.Sprint(line["tools_json"]), wantTools(tc.p); got != want {
				t.Errorf("tools:\n got %q\nwant %q", got, want)
			}
		})
	}
	t.Run("no tools", func(t *testing.T) {
		line := fullRequestLine(t, []spawnllm.Message{{Role: "user", Content: "go"}})
		if got := fmt.Sprint(line["tools_json"]); got != "[]" {
			t.Errorf("tools = %q, want a request with no tools rendered as []", got)
		}
	})
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
		setTestProvider(agent, &sequenceProvider{responses: []*spawnllm.LLMResponse{{Content: "ok"}}, errors: []error{nil}})
		opts := processOptions{SessionKey: "request-log", Channel: "cli", ChatID: "direct", UserMessage: "go"}
		cm, release := al.getContextManager(agent, opts.SessionKey)
		_, err := al.runLLMIteration(context.Background(), agent, []spawnllm.Message{{Role: "user", Content: long}}, opts, cm, nil)
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
