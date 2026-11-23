// ClawEh
// License: MIT

package agent

import (
	"strings"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// cmdsEdgeFromU1 is a message from the telegram user u1, whose chat routes to
// Bob in messagingConfig.
func cmdsEdgeFromU1(text string) bus.InboundMessage {
	return bus.InboundMessage{
		Channel: "telegram", ChatID: "chat-u1", SenderID: "u1", MessageID: "m-1",
		Content: text, Peer: bus.Peer{Kind: "direct", ID: "u1"},
	}
}

// TestCommandSender_Names: the WebUI user, a sender with a name, one with
// only an id, and one with neither, which is named by its channel.
func TestCommandSender_Names(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  bus.InboundMessage
		want string
	}{
		{"webui", bus.InboundMessage{Channel: webUIChannel, SenderID: "x1", Sender: bus.SenderInfo{DisplayName: "Alice"}}, webUISender},
		{"named", bus.InboundMessage{Channel: "telegram", SenderID: "u1", Sender: bus.SenderInfo{DisplayName: "Alice"}}, "Alice"},
		{"id only", bus.InboundMessage{Channel: "telegram", SenderID: "u1"}, "u1"},
		{"nothing", bus.InboundMessage{Channel: "telegram"}, "telegram"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandSender(tc.msg); got != tc.want {
				t.Fatalf("commandSender = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCommandAskReply_EmptyReply: an agent that answers with nothing is
// reported as giving no reply.
func TestCommandAskReply_EmptyReply(t *testing.T) {
	if got := commandAskReply("Bob", tools.AgentReply{Outcome: bus.OutcomeOK, Text: "  \n"}, nil); got != "Bob gave no reply." {
		t.Fatalf("commandAskReply = %q", got)
	}
}

// TestCommandWhisper_Fails: a whisper the agent cannot take is one plain
// sentence naming it, and the reason is logged.
func TestCommandWhisper_Fails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := messagingLoop(t, messagingConfig(t), &recordingProvider{})

	if got := al.commandWhisper(t.Context(), cmdsEdgeFromU1("/whisper bob"), "bob", ""); got != "Could not whisper to Bob." {
		t.Fatalf("commandWhisper = %q", got)
	}
	if !strings.Contains(logs.String(), "/whisper failed") {
		t.Fatalf("the failed whisper was not logged:\n%s", logs.String())
	}
}

// TestCommandAsk_Fails: an ask that fails posts one plain sentence naming the
// agent to the chat that sent /ask and logs the reason.
func TestCommandAsk_Fails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := messagingLoop(t, messagingConfig(t), &recordingProvider{})
	al.running.Store(false) // the ask is refused: the service is not running

	if got := al.commandAsk(t.Context(), cmdsEdgeFromU1("/ask bob hi"), "bob", "hi"); got != "" {
		t.Fatalf("commandAsk = %q, want nothing now", got)
	}
	out := nextOutbound(t, msgBus)
	if out.Content != "Could not ask Bob." || out.Channel != "telegram" || out.ChatID != "chat-u1" || out.OriginalMessageID != "m-1" {
		t.Fatalf("reply = %+v", out)
	}
	if !strings.Contains(logs.String(), "/ask failed") || !strings.Contains(logs.String(), "the service is not running") {
		t.Fatalf("the failed ask was not logged:\n%s", logs.String())
	}
}

// TestCommandAsk_ReplyNotPosted: an ask whose reply cannot be posted is
// logged.
func TestCommandAsk_ReplyNotPosted(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := messagingLoop(t, messagingConfig(t), &recordingProvider{})
	al.running.Store(false)
	msgBus.Close()

	if got := al.commandAsk(t.Context(), cmdsEdgeFromU1("/ask bob hi"), "bob", "hi"); got != "" {
		t.Fatalf("commandAsk = %q, want nothing now", got)
	}
	eventually(t, "the unposted reply to be logged", func() bool {
		return strings.Contains(logs.String(), "Failed to post the /ask reply")
	})
}
