// ClawEh
// License: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// longJobModel starts a long background shell job when a person writes.
type longJobModel struct{}

func (longJobModel) GetDefaultModel() string { return "mock-model" }

func (longJobModel) Chat(_ context.Context, _ providers.Identity, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	if messages[len(messages)-1].Role == "tool" {
		return &providers.LLMResponse{Content: "started"}, nil
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID: "tc-1", Type: "function", Name: "shell_exec",
		Function: &providers.FunctionCall{Name: "shell_exec", Arguments: `{"command":"sleep 60","background":true,"notify":false}`},
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
	model := chatFunc(func(ctx context.Context, messages []providers.Message) (*providers.LLMResponse, error) {
		switch user := lastUser(messages); {
		case user == "the work" && messages[len(messages)-1].Role != "tool":
			return &providers.LLMResponse{Content: "first part", ToolCalls: []providers.ToolCall{{
				ID: "tc-w", Type: "function", Name: "time_now",
				Function: &providers.FunctionCall{Name: "time_now", Arguments: `{}`},
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
			return &providers.LLMResponse{Content: "started"}, nil
		default:
			return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
				ID: "tc-1", Type: "function", Name: "agent_spawn",
				Function: &providers.FunctionCall{Name: "agent_spawn", Arguments: `{"task":"the work","name":"job"}`},
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
	taskID := al.tasks.Running("alice")[0].ID
	send("m2", "hold on")
	awaitSignal(t, replyStarted, "the reply to start")
	send("m3", "/cancel all")
	if out := nextOutbound(t, msgBus); out.OriginalMessageID != "m3" || out.Content != "Alice: stopped, and cancelled 1 background task." {
		t.Fatalf("/cancel all reply = %+v", out)
	}
	if running := al.tasks.Running("alice"); len(running) != 0 {
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
