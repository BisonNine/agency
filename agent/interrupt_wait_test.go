// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// Rule 5: /cancel while the reply waits on a sub-agent does not stop the
// sub-agent. The turn stops waiting; the sub-agent finishes, and its result
// arrives as a background result that starts a new turn of the agent.
func TestInterrupt_WaitTaskFinishesAndReports(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))

	workerStarted := make(chan struct{}, 1)
	releaseWorker := make(chan struct{})
	reported := make(chan string, 1)
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

	publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: "m1", Content: "do it"})
	select {
	case <-workerStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the sub-agent did not start")
	}
	publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: "m2", Content: "/cancel"})
	// The turn stops waiting: the task is now a background one.
	deadline := time.Now().Add(30 * time.Second)
	for {
		running := al.tasks.Running("alice")
		if len(running) == 1 && running[0].Mode == hook.TaskCallback {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the waiting task was not detached: %+v", running)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(releaseWorker)
	select {
	case got := <-reported:
		if !strings.Contains(got, "status: done") {
			t.Fatalf("background result = %q", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the sub-agent's result never reached the agent")
	}
	al.activeRequests.Wait()
}
