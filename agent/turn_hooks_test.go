// ClawEh
// License: MIT

package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/tools"
)

// At the end of the model loop the stream is flushed before the eviction
// notice is posted, and the notice before the progress heartbeat stops.
func TestEndTurn_FlushThenNoticeThenStop(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al := tl.al
	agent := al.registry.Default()
	notify := true
	agent.Config.ContextEviction = &config.ContextEvictionConfig{NotifyUser: &notify}
	opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "c1"}
	h, st := al.buildTurnHooks(context.Background(), agent, opts, "m")

	// noticeQueued waits up to d for an outbound message (the notice).
	noticeQueued := func(d time.Duration) bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, ok := tl.msgBus.SubscribeOutbound(ctx)
		return ok
	}
	var steps []string
	st.stream = newStreamCoalescer(func(string) {
		if noticeQueued(20 * time.Millisecond) {
			t.Error("the eviction notice was posted before the stream was flushed")
		}
		steps = append(steps, "flush")
	})
	st.stream.Add("Hi Bob")
	st.stopProgress = func() {
		if !noticeQueued(2 * time.Second) {
			t.Error("the heartbeat stopped before the eviction notice was posted")
		}
		steps = append(steps, "stop")
	}

	h.emit(context.Background(), hook.Compaction{Phase: hook.CompactionEvicted, Evictions: []hook.Eviction{{Tool: "file_read_lines", Resource: "notes.md", Bytes: 2048}}})
	h.emit(context.Background(), hook.TurnFinished{})
	if !slices.Equal(steps, []string{"flush", "stop"}) {
		t.Fatalf("steps = %v", steps)
	}
}

// Each provider call writes its finish line; an answer also adds to the
// turn's usage, is dumped and has its reasoning delivered.
func TestModelCall_FinishUsageDumpsReasoning(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	tl := newTestAgentLoop(t)
	al := tl.al
	al.dumpsDir = dump.Dir{Path: t.TempDir()}
	al.cfg.Logging.DumpAll = true
	agent := al.registry.Default()
	usage := &global.TurnUsage{}
	opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "c1", UsageOut: usage}
	al.setExposeReasoning(agent, opts.SessionKey, true)
	h := al.newTurnHooks(context.Background(), agent, opts, "m")

	h.emit(context.Background(), hook.ModelCall{
		Iteration: 1, Model: hook.ModelRef{Name: "m", Provider: "Alice's API", Model: "m-wire"},
		Request:  &hook.ModelRequest{Messages: []providers.Message{{Role: "user", Content: "hi"}}},
		Response: &providers.LLMResponse{Content: "hello", Reasoning: "Bob asked", Status: &providers.DispatchStatus{Success: true, Model: "m-wire"}},
	})

	if !strings.Contains(buf.String(), "LLM finish") {
		t.Error("no finish line")
	}
	if usage.Provider != "Alice's API" || usage.Model != "m-wire" {
		t.Errorf("usage = %+v, want the answering call's", usage)
	}
	if entries, err := os.ReadDir(al.dumpsDir.Path); err != nil || len(entries) == 0 {
		t.Error("no dump written")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, ok := tl.msgBus.SubscribeOutbound(ctx)
	if !ok || !strings.Contains(msg.Content, "Bob asked") {
		t.Errorf("reasoning delivered = %q, %v", msg.Content, ok)
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

// A step of the end of the loop that panics still stops the heartbeat, whose
// goroutine would otherwise keep editing the placeholder.
func TestEndTurn_PanicStillStopsTheHeartbeat(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	agent := tl.al.registry.Default()
	h, st := tl.al.buildTurnHooks(context.Background(), agent, processOptions{SessionKey: "s", Channel: "telegram", ChatID: "c1"}, "m")
	st.stream = newStreamCoalescer(func(string) { panic("Bob's bug") })
	st.stream.Add("Hi Bob")
	stopped := false
	st.stopProgress = func() { stopped = true }
	h.emit(context.Background(), hook.TurnFinished{})
	if !stopped {
		t.Fatal("the heartbeat was not stopped")
	}
}
