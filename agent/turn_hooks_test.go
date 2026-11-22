// ClawEh
// License: MIT

package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/tools"
)

// streamingEchoChannel is the echo channel with partial-text streaming.
type streamingEchoChannel struct{ echoChannel }

func (*streamingEchoChannel) StreamDelta(context.Context, string, string) error { return nil }

// The turn's callbacks and consumers are registered in one place, in one
// order, and the order is behaviour:
//
//   - TurnFinished: the stream is flushed, then the eviction notice posted,
//     then the heartbeat stopped, as the model loop's deferred steps always
//     ran;
//   - ModelCall: the finish line, the usage, the dumps, then the reasoning;
//   - AfterModel: generated images are handled before reasoning may stand in
//     for an empty reply, and both before the assistant message is stored
//     (TestEngine_AfterModelBeforeStorage);
//   - ToolCallFinished: statistics then the audit row, after the call; no
//     BeforeTool audits a call before it runs, and no AfterTool or
//     OnToolError changes a result: tool output is stored as the tool
//     returned it, and redaction stays on logged and rendered error text.
func TestTurnHooksOrder(t *testing.T) {
	tl := newTestAgentLoop(t)
	al := tl.al
	withEchoChannel(t, al, tl.msgBus)
	streaming := &streamingEchoChannel{}
	al.channelManager.RegisterChannel("stream", streaming)

	agent := al.registry.Default()
	opts := processOptions{SessionKey: "s", Channel: "stream", ChatID: "c1"}
	al.setShowToolActivity(agent, opts.SessionKey, true)

	h := al.newTurnHooks(context.Background(), agent, opts, "m")
	want := []string{
		"OnTextDelta:stream", "OnEvent:stream",
		"OnEvent:llm-finish", "OnEvent:usage", "OnEvent:dumps", "OnEvent:reasoning",
		"OnFallback:fallback-notice",
		"OnCompaction:compaction-notice",
		"AfterModel:images", "AfterModel:reasoning-as-content",
		"OnEvent:breadcrumbs",
		"OnEvent:tool-record",
		"OnEvent:evictions",
		"OnEvent:progress",
		"Interim.ToolsReturned:progress",
		"Interim.Narrate:narration",
		"Interim.ToolsReturned:tool-output", "Interim.ToolResult:tool-output",
		"Interim.DescribeImages:vision-describe",
	}
	if !slices.Equal(h.order, want) {
		t.Fatalf("registration order:\n got %s\nwant %s", strings.Join(h.order, ", "), strings.Join(want, ", "))
	}
	for _, kind := range []string{"BeforeTool:", "AfterTool:", "OnToolError:"} {
		if slices.ContainsFunc(h.order, func(s string) bool { return strings.HasPrefix(s, kind) }) {
			t.Errorf("a %s callback is registered: tool calls and results are not changed today", strings.TrimSuffix(kind, ":"))
		}
	}

	// A turn with no chat to stream to, show tools to or warn of a fallback
	// in registers neither, and the others in the same order.
	quiet := al.newTurnHooks(context.Background(), agent, processOptions{SessionKey: "s2", Channel: "system", ChatID: "c1"}, "m")
	var rest []string
	for _, s := range want {
		if !strings.HasSuffix(s, ":stream") && s != "OnEvent:breadcrumbs" && s != "OnFallback:fallback-notice" {
			rest = append(rest, s)
		}
	}
	if !slices.Equal(quiet.order, rest) {
		t.Fatalf("quiet turn order:\n got %s\nwant %s", strings.Join(quiet.order, ", "), strings.Join(rest, ", "))
	}
}

// A finished tool call is recorded in the session's statistics and then the
// audit log; a call whose tool panicked counts as its error text and has no
// audit row, as before.
func TestRecordToolCall_StatsThenAudit(t *testing.T) {
	if err := audit.Init(t.TempDir()); err != nil {
		t.Fatalf("audit.Init: %v", err)
	}
	t.Cleanup(func() {
		if err := audit.Close(); err != nil {
			t.Errorf("audit.Close: %v", err)
		}
	})
	agent := &AgentInstance{ID: "alice"}
	opts := processOptions{SessionKey: "agent:alice:record", Channel: "slack", ChatID: "C1"}
	tools.BeginToolStats(opts.SessionKey)

	recordToolCall(context.Background(), agent, opts, hook.ToolCallFinished{
		Call: hook.ToolCall{ID: "c1", Name: "file_read_lines"}, Result: tools.SilentResult("ok"),
	})
	recordToolCall(context.Background(), agent, opts, hook.ToolCallFinished{
		Call: hook.ToolCall{ID: "c2", Name: "boom"},
		Result: &tools.ToolResult{
			Err:    &toolPanicError{tool: "boom", value: "Bob's bug"},
			ForLLM: "Tool boom failed with an internal error.",
		},
	})

	calls, errs, last := tools.EndToolStats(opts.SessionKey)
	if calls != 2 || errs != 1 || last != "boom: Tool boom failed with an internal error." {
		t.Fatalf("stats = %d calls, %d errors, last %q", calls, errs, last)
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := audit.Default().Flush(flushCtx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows, err := audit.Default().Query(context.Background(), audit.Filter{Kind: audit.KindToolCall})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 || rows[0].Tool != "file_read_lines" {
		t.Fatalf("audit rows = %+v, want the one call that ran", rows)
	}
}
