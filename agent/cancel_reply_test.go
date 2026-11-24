// Agency
// License: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	toolsagents "go.bison9.com/agency/tools/agents"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// longJobModel starts a long background shell job when a person writes.
type longJobModel struct{}

func (longJobModel) GetDefaultModel() string { return "mock-model" }

func (longJobModel) Chat(_ context.Context, _ accounting.Identity, messages []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	if messages[len(messages)-1].Role == "tool" {
		return &spawnllm.LLMResponse{Content: "started"}, nil
	}
	return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
		ID: "tc-1", Type: "function", Name: "shell_exec",
		Function: &spawnllm.FunctionCall{Name: "shell_exec", Arguments: `{"command":"sleep 60","background":true,"notify":false}`},
	}}}, nil
}

// Shell jobs are background work: /cancel counts them and leaves them
// running; /cancel all stops them.
func TestCancel_ShellJobsCountedAndStopped(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	t.Cleanup(func() { toolsshell.Host.StopAll("alice") })
	cfg := messagingConfig(t)
	cfg.Agents.List[0].Tools = []string{config.ShellExecTool}
	al, msgBus := messagingLoop(t, cfg, longJobModel{})

	send := func(id, text string) string {
		t.Helper()
		msg := inbound("chat-alice", id, text)
		msg.Metadata = map[string]string{metadataKeyPreresolvedAgentID: "alice"}
		dispatch(al, msg)
		return nextOutboundTo(t, msgBus, "chat-alice").Content
	}
	if got := send("m1", "build it"); got != "started" {
		t.Fatalf("first reply = %q", got)
	}
	if n := toolsshell.Host.Running("alice"); n != 1 {
		t.Fatalf("running jobs = %d, want 1", n)
	}
	if got := send("m2", "/cancel"); got != "Alice: nothing to stop. 1 background task is still running — /cancel all stops it too." {
		t.Fatalf("/cancel reply = %q", got)
	}
	if n := toolsshell.Host.Running("alice"); n != 1 {
		t.Fatalf("/cancel stopped the job: %d running", n)
	}
	if got := send("m3", "/cancel all"); got != "Alice: cancelled 1 background task." {
		t.Fatalf("/cancel all reply = %q", got)
	}
	if n := toolsshell.Host.Running("alice"); n != 0 {
		t.Fatalf("/cancel all left %d jobs running", n)
	}
}

// /cancel all stops the running reply and every background task of the
// agent, with the tasks they started: the record says cancelled and no
// result comes back.
func TestCancelAll_StopsTheReplyAndTheTasks(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	workerStarted := make(chan struct{}, 4)
	replyStarted := make(chan struct{}, 4)
	model := chatFunc(func(ctx context.Context, messages []spawnllm.Message) (*spawnllm.LLMResponse, error) {
		switch user := lastUser(messages); {
		case user == "the work" && messages[len(messages)-1].Role != "tool":
			return &spawnllm.LLMResponse{Content: "first part", ToolCalls: []spawnllm.ToolCall{{
				ID: "tc-w", Type: "function", Name: "time_now",
				Function: &spawnllm.FunctionCall{Name: "time_now", Arguments: `{}`},
			}}}, nil
		case user == "the work":
			workerStarted <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		case user == "hold on":
			replyStarted <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		case messages[len(messages)-1].Role == "tool":
			return &spawnllm.LLMResponse{Content: "started"}, nil
		default:
			return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
				ID: "tc-1", Type: "function", Name: "agent_spawn",
				Function: &spawnllm.FunctionCall{Name: "agent_spawn", Arguments: `{"task":"the work","name":"job"}`},
			}}}, nil
		}
	})
	al, msgBus := messagingLoop(t, messagingConfig(t), model)
	unwound := make(chan struct{}, 4)
	al.tasks.Observe(func(info hook.TaskInfo) {
		if info.State == hook.TaskCancelled {
			unwound <- struct{}{}
		}
	})
	send := func(id, text string) {
		t.Helper()
		publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: id, Content: text})
	}
	send("m1", "start it")
	if out := nextOutbound(t, msgBus); out.Content != "started" {
		t.Fatalf("first reply = %+v", out)
	}
	awaitSignal(t, workerStarted, "the sub-agent to start")
	taskID := toolsagents.RunningTasks(al.tasks, "alice")[0].ID
	send("m2", "hold on")
	awaitSignal(t, replyStarted, "the reply to start")
	send("m3", "/cancel all")
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m3" || out.Content != "Alice: stopped, and cancelled 1 background task." {
		t.Fatalf("/cancel all reply = %+v", out)
	}
	if running := toolsagents.RunningTasks(al.tasks, "alice"); len(running) != 0 {
		t.Fatalf("tasks still running: %+v", running)
	}
	al.activeRequests.Wait()
	noOutbound(t, msgBus)

	// Once the worker has unwound, its partial output is kept, marked
	// incomplete.
	recv(t, unwound, "the cancel")
	recv(t, unwound, "the worker to unwind")
	alice, _ := al.GetRegistry().Get("alice")
	b, err := os.ReadFile(filepath.Join(alice.Workspace, "tasks", taskID+"-results.json"))
	if err != nil || !strings.Contains(string(b), "first part") ||
		!strings.Contains(string(b), `"incomplete": true`) || !strings.Contains(string(b), `"status": "cancelled"`) {
		t.Fatalf("results = %s (%v)", b, err)
	}
}

// /cancel stops the running reply and leaves the task that reply started
// running: the turn is interrupted, never cancelled with its tasks.
func TestCancel_LeavesTheTurnsOwnTaskRunning(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	workerStarted := make(chan context.Context, 4)
	replyWaiting := make(chan struct{}, 4)
	model := chatFunc(func(ctx context.Context, messages []spawnllm.Message) (*spawnllm.LLMResponse, error) {
		switch user := lastUser(messages); {
		case user == "the work":
			workerStarted <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		case messages[len(messages)-1].Role == "tool":
			// The reply goes on after starting the task, until stopped.
			replyWaiting <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
				ID: "tc-1", Type: "function", Name: "agent_spawn",
				Function: &spawnllm.FunctionCall{Name: "agent_spawn", Arguments: `{"task":"the work","name":"job"}`},
			}}}, nil
		}
	})
	al, msgBus := messagingLoop(t, messagingConfig(t), model)
	send := func(id, text string) {
		t.Helper()
		publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: id, Content: text})
	}
	send("m1", "start it")
	var workerCtx context.Context
	select {
	case workerCtx = <-workerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the sub-agent did not start")
	}
	awaitSignal(t, replyWaiting, "the reply to go on")

	send("m2", "/cancel")
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m2" ||
		out.Content != "Alice: stopped. 1 background task is still running — /cancel all stops it too." {
		t.Fatalf("/cancel reply = %+v", out)
	}
	if running := toolsagents.RunningTasks(al.tasks, "alice"); len(running) != 1 {
		t.Fatalf("running tasks = %+v, want the one the reply started", running)
	}
	if workerCtx.Err() != nil {
		t.Fatalf("the task was stopped with the reply: %v", context.Cause(workerCtx))
	}

	send("m3", "/cancel all")
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m3" || out.Content != "Alice: cancelled 1 background task." {
		t.Fatalf("/cancel all reply = %+v", out)
	}
	al.activeRequests.Wait()
}

// A config reload leaves a running task running, and the reloaded agent
// lists it and cancels it.
func TestReload_TaskSurvivesAndIsCancellable(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	workerStarted := make(chan context.Context, 4)
	model := chatFunc(func(ctx context.Context, messages []spawnllm.Message) (*spawnllm.LLMResponse, error) {
		switch user := lastUser(messages); {
		case user == "the work":
			workerStarted <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		case messages[len(messages)-1].Role == "tool":
			return &spawnllm.LLMResponse{Content: "started"}, nil
		default:
			return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
				ID: "tc-1", Type: "function", Name: "agent_spawn",
				Function: &spawnllm.FunctionCall{Name: "agent_spawn", Arguments: `{"task":"the work","name":"job"}`},
			}}}, nil
		}
	})
	cfg := messagingConfig(t)
	al, msgBus := messagingLoop(t, cfg, model)
	send := func(id, text string) {
		t.Helper()
		publishIn(t, msgBus, bus.InboundMessage{Channel: "test", ChatID: "c1", SenderID: "u1", MessageID: id, Content: text})
	}
	send("m1", "start it")
	if out := nextOutbound(t, msgBus); out.Content != "started" {
		t.Fatalf("first reply = %+v", out)
	}
	var workerCtx context.Context
	select {
	case workerCtx = <-workerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the sub-agent did not start")
	}
	before, _ := al.GetRegistry().Get("alice")

	next := *cfg
	if err := reloadForTest(t, al, context.Background(), model, &next); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	after, _ := al.GetRegistry().Get("alice")
	if after == before {
		t.Fatal("the reload kept Alice's instance")
	}
	setTestProvider(after, model)
	if workerCtx.Err() != nil {
		t.Fatalf("the reload stopped the task: %v", context.Cause(workerCtx))
	}
	if running := toolsagents.RunningTasks(al.tasks, "alice"); len(running) != 1 {
		t.Fatalf("running tasks after the reload = %+v, want the task", running)
	}

	send("m2", "/cancel all")
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m2" || out.Content != "Alice: cancelled 1 background task." {
		t.Fatalf("/cancel all reply = %+v", out)
	}
	if workerCtx.Err() == nil {
		t.Fatal("the task still runs after /cancel all")
	}
	al.activeRequests.Wait()
}
