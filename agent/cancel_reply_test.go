// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"

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
	if got := send("m2", "/cancel"); got != "No pending messages to cancel. 1 background task is still running — /cancel all stops it too." {
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
