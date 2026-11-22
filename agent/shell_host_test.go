// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// jobModel starts a background shell job when a person writes, and answers
// a shell job notice by quoting it.
type jobModel struct {
	mu      sync.Mutex
	notices []string
}

func (m *jobModel) GetDefaultModel() string { return "mock-model" }

func (m *jobModel) Chat(_ context.Context, _ providers.Identity, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	last := messages[len(messages)-1]
	switch {
	case last.Role == "tool":
		return &providers.LLMResponse{Content: "started"}, nil
	case strings.Contains(last.Content, "Shell job "):
		m.mu.Lock()
		m.notices = append(m.notices, last.Content)
		m.mu.Unlock()
		return &providers.LLMResponse{Content: "notice: " + last.Content}, nil
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID: "tc-1", Type: "function", Name: "shell_exec",
		Function: &providers.FunctionCall{Name: "shell_exec", Arguments: `{"command":"echo alice-bg-done","background":true}`},
	}}}, nil
}

// TestShell_JobNoticeReachesTheStartingChat: the end of a background job
// reaches the agent's conversation as a background result, through the
// same callback a callback sub-agent's result takes, and the agent's answer
// goes to the chat the job was started from.
func TestShell_JobNoticeReachesTheStartingChat(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	t.Cleanup(func() { toolsshell.Host.StopAll("alice") })
	model := &jobModel{}
	cfg := messagingConfig(t)
	cfg.Agents.List[0].Tools = []string{config.ShellExecTool}
	al, msgBus := messagingLoop(t, cfg, model)

	msg := inbound("chat-alice", "m1", "build it")
	msg.Metadata = map[string]string{metadataKeyPreresolvedAgentID: "alice"}
	dispatch(al, msg)
	if out := nextOutboundTo(t, msgBus, "chat-alice"); out.Content != "started" {
		t.Fatalf("first reply = %q", out.Content)
	}
	out := nextOutboundTo(t, msgBus, "chat-alice")
	if !strings.HasPrefix(out.Content, "notice: [System: async:shell_exec] Shell job ") ||
		!strings.Contains(out.Content, "exited with code 0") || !strings.Contains(out.Content, "alice-bg-done") {
		t.Errorf("notice reply = %q", out.Content)
	}
}

// TestShell_LostJobNoticeGoesToTheMainConversation: a notice no call carries
// (a job lost in a restart) reaches the agent's main conversation; its
// answer goes to the agent's default chat when it has one.
func TestShell_LostJobNoticeGoesToTheMainConversation(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	model := &jobModel{}
	cfg := messagingConfig(t)
	cfg.Bindings[0].Default = true // Bob's default chat
	al, msgBus := messagingLoop(t, cfg, model)

	text := "Shell job j0123abcd was lost: the service restarted while it ran. It was not restarted."
	al.ShellHooks().Deliver("bob", text)
	out := nextOutboundTo(t, msgBus, "u1")
	if out.Channel != "telegram" || out.Content != "notice: [System: async:shell_exec] "+text {
		t.Errorf("reply = %+v", out)
	}
}
