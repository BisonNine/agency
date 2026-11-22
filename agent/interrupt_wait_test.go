// ClawEh
// License: MIT

package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/tools"
)

// Rule 5: /cancel while the reply waits on a sub-agent does not stop the
// sub-agent. The turn stops waiting; the sub-agent finishes, and its result
// arrives as a background result that starts a new turn of the agent.
func TestInterrupt_WaitTaskFinishesAndReports(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))

	workerStarted := make(chan struct{}, 1)
	releaseWorker := make(chan struct{})
	reported := make(chan string, 4)
	model := chatFunc(func(ctx context.Context, messages []providers.Message) (*providers.LLMResponse, error) {
		user := lastUser(messages)
		last := messages[len(messages)-1]
		switch {
		case user == "the work":
			// The worker: runs until released, whatever its caller does.
			workerStarted <- struct{}{}
			select {
			case <-releaseWorker:
				return &providers.LLMResponse{Content: "work finished"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case strings.Contains(user, "TASK NOTIFICATION"):
			reported <- user
			return &providers.LLMResponse{Content: "the background work is done"}, nil
		case last.Role == "tool":
			return &providers.LLMResponse{Content: "unexpected: the turn went on"}, nil
		default:
			return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
				ID: "tc-1", Type: "function", Name: "agent_spawn",
				Function: &providers.FunctionCall{Name: "agent_spawn", Arguments: `{"task":"the work","mode":"wait"}`},
			}}}, nil
		}
	})
	cfg := messagingConfig(t)
	al, msgBus := messagingLoop(t, cfg, model)
	detached := make(chan struct{}, 1)
	al.tasks.Observe(func(info hook.TaskInfo) {
		if info.State == hook.TaskRunning && info.Mode == hook.TaskCallback {
			detached <- struct{}{}
		}
	})

	publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: "m1", Content: "do it"})
	awaitSignal(t, workerStarted, "the sub-agent to start")
	publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: "m2", Content: "/cancel"})
	// One reply, naming the task still running.
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m2" ||
		out.Content != "Alice: stopped. 1 background task is still running — /cancel all stops it too." {
		t.Fatalf("cancel reply = %+v", out)
	}
	// The turn stops waiting: the task is now a background one.
	awaitSignal(t, detached, "the waiting task to be detached")

	close(releaseWorker)
	got := <-reported
	if !strings.Contains(got, "status: done") {
		t.Fatalf("background result = %q", got)
	}
	al.activeRequests.Wait()
	// After the cancel reply: the task's notification and the agent's
	// answer to its result, and nothing else (the stopped message has no
	// reply of its own).
	var after []string
	noOutboundExcept(t, msgBus, func(out bus.OutboundMessage) bool {
		after = append(after, out.Content)
		return true
	})
	if len(after) != 2 || !strings.Contains(after[0], "TASK NOTIFICATION") || after[1] != "the background work is done" {
		t.Fatalf("messages after the cancel reply = %q", after)
	}
	if len(reported) != 0 {
		t.Fatal("the detached result was delivered more than once")
	}
}

// A CLI model's agent_spawn arrives over MCP on a request context that a
// shutdown ends with a plain cancel. A wait task whose turn stops waiting
// once the loop is stopping is cancelled, not detached, so after the
// restart it is not relaunched beside the replayed turn.
func TestInterrupt_WaitTaskDuringShutdownIsCancelled(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	workerStarted := make(chan struct{}, 1)
	model := chatFunc(func(ctx context.Context, messages []providers.Message) (*providers.LLMResponse, error) {
		workerStarted <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	al, _ := messagingLoop(t, messagingConfig(t), model)
	alice, ok := al.GetRegistry().Get("alice")
	if !ok || alice.spawnMgr == nil {
		t.Fatal("alice has no spawn manager")
	}
	reqCtx, endRequest := context.WithCancel(context.Background())
	done := make(chan *tools.ToolResult, 1)
	go func() {
		res, err := alice.spawnMgr.Run(reqCtx, "the work", "job", "", "cli", "direct", "", "", nil,
			func(context.Context, *tools.ToolResult) { t.Error("a background result was delivered") })
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()
	awaitSignal(t, workerStarted, "the sub-agent to start")
	al.Stop()
	endRequest() // the killed CLI's MCP request ends: a plain cancel
	if res := <-done; !res.IsError || !strings.Contains(res.ForLLM, "was cancelled") {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	if markers, _ := filepath.Glob(filepath.Join(alice.Workspace, "tasks", "*.run")); len(markers) != 0 {
		t.Fatalf("run markers = %v: the task would be relaunched", markers)
	}
}
