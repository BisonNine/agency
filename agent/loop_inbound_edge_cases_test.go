// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
)

// An external message carries the configured prefix in place of the default.
func TestInbound_ExternalMessagePrefix(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus := hooksEdgeLoop(t, func(cfg *config.Config) {
		setDefaultBinding(cfg, "main", "telegram", "direct", "42")
		cfg.Security.MessagePrefix = "[hook] "
	})
	if err := al.HandleExternalMessage(context.Background(), "main", "build done"); err != nil {
		t.Fatal(err)
	}
	msg, ok := consumeInbound(t, msgBus)
	if !ok || msg.Content != "[hook] build done" || msg.ChatID != "42" || strings.HasPrefix(msg.Content, global.DefaultMessagePrefix) {
		t.Fatalf("inbound = %+v, want the configured prefix", msg)
	}
}

// A direct message to a chat with no peer kind routes as a channel peer.
func TestInbound_DirectDefaultsToChannelPeer(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t, func(cfg *config.Config) {
		cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "ops", Name: "Ops"})
		setDefaultBinding(cfg, "ops", "telegram", "channel", "ops-room")
	})
	reply, err := al.ProcessDirectWithChannel(context.Background(), "status?", "", "telegram", "ops-room", "")
	if err != nil || reply != "Mock response" {
		t.Fatalf("ProcessDirectWithChannel = %q, %v", reply, err)
	}
	ops, _ := al.GetRegistry().Get("ops")
	if !historyContains(ops, routing.BuildAgentMainSessionKey("ops"), "status?") {
		t.Fatal("the message did not reach the agent bound to the channel peer")
	}
}

// inboundEdgePanicProvider panics on every call.
type inboundEdgePanicProvider struct{}

func (inboundEdgePanicProvider) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	panic("Bob's provider bug")
}

func (inboundEdgePanicProvider) GetDefaultModel() string { return "test-model" }

// A panic in the turn is returned as an error naming it and logged.
func TestInbound_PanicBecomesError(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), inboundEdgePanicProvider{}, nil)
	resp, err := al.processMessageSafely(context.Background(), inbound("c1", "m1", "hi"), nil)
	if resp != "" || err == nil || !strings.Contains(err.Error(), "internal error while processing the message") {
		t.Fatalf("processMessageSafely = %q, %v; want the panic as an error", resp, err)
	}
	if !strings.Contains(logs.String(), "panic while processing message") {
		t.Fatalf("the panic was not logged: %s", logs.String())
	}
}

// A reply to an honored mention is attributed to the agent, by id when it
// has no name.
func TestInbound_MentionAttributedByID(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t, func(cfg *config.Config) {
		cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "ops"})
		cfg.Bindings = []config.AgentBinding{{AgentID: "main", AgentMentions: []string{"*"}, Match: config.BindingMatch{Channel: "test"}}}
	})
	reply, err := al.processMessage(context.Background(), inbound("c1", "m1", "@ops status?"))
	if err != nil || reply != "**ops:**\nMock response" {
		t.Fatalf("processMessage = %q, %v; want the reply attributed to ops", reply, err)
	}
}

// With message content logging on, an inbound message is previewed: in full
// when it reports an error, else truncated.
func TestInbound_LogPreview(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	logger.SetLogMessageContent(true)
	t.Cleanup(func() { logger.SetLogMessageContent(false) })
	long := strings.Repeat("x", 200)
	logInboundMessage(context.Background(), bus.InboundMessage{Channel: "telegram", SenderID: "s", Content: "Error: " + long})
	logInboundMessage(context.Background(), bus.InboundMessage{Channel: "telegram", SenderID: "s", Content: "plain " + long})
	out := logs.String()
	if !strings.Contains(out, "Error: "+long) {
		t.Fatal("an error message was not previewed in full")
	}
	if strings.Contains(out, "plain "+long) || !strings.Contains(out, "plain xxx") {
		t.Fatalf("an ordinary message was not truncated: %s", out)
	}
}

// inboundEdgeMessageTool is a "message" tool that counts its round resets.
type inboundEdgeMessageTool struct{ resets atomic.Int32 }

func (*inboundEdgeMessageTool) Name() string               { return "message" }
func (*inboundEdgeMessageTool) Description() string        { return "test message tool" }
func (*inboundEdgeMessageTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (*inboundEdgeMessageTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return &toolkit.Result{ForLLM: "ok"}
}
func (m *inboundEdgeMessageTool) ResetSentInRound() { m.resets.Add(1) }

// The agent's message tool has its round reset.
func TestInbound_ResetMessageToolRound(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t)
	tool := &inboundEdgeMessageTool{}
	registerOn(t, al, "main", tool)
	resetMessageToolRound(al.GetRegistry().Default())
	if n := tool.resets.Load(); n != 1 {
		t.Fatalf("resets = %d, want 1", n)
	}
}
