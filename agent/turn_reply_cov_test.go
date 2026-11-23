// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
)

// A reply about an agent with no id still says it is gone.
func TestTurnReplyCov_AgentGoneText(t *testing.T) {
	if got := agentGoneText(""); got != "That agent no longer exists." {
		t.Fatalf("agentGoneText = %q", got)
	}
}

// turnReplyCovToBob is a message to the human agent bob outside the ask
// channel.
func turnReplyCovToBob(required bool) bus.InboundMessage {
	msg := bus.InboundMessage{
		Channel: "telegram", ChatID: "42", MessageID: "m1", SenderID: "carol", Content: "hi",
		Metadata: map[string]string{metadataKeyPreresolvedAgentID: "bob"},
	}
	if required {
		msg.Metadata[bus.MetaReplyRequired] = "1"
	}
	return msg
}

// A request to a person outside the ask channel, ended by shutdown, is
// answered as cancelled only when its sender requires a reply; a reply the
// bus refuses is logged.
func TestTurnReplyCov_ShutdownEndsPersonRequest(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus, _ := newHumanLoop(t, 60)
	ctx := context.Background()

	al.endTurnOnShutdown(ctx, ctx, turnReplyCovToBob(false))
	noOutbound(t, msgBus)

	al.endTurnOnShutdown(ctx, ctx, turnReplyCovToBob(true))
	got := nextOutbound(t, msgBus)
	if got.ChatID != "42" || got.OriginalMessageID != "m1" || got.Outcome != bus.OutcomeCancelled ||
		got.Content != "The request was cancelled because the service is shutting down." {
		t.Fatalf("reply = %+v, want the shutdown cancellation", got)
	}

	msgBus.Close()
	al.endTurnOnShutdown(ctx, ctx, turnReplyCovToBob(true))
	if !strings.Contains(logs.String(), "Failed to publish cancelled reply") {
		t.Fatalf("the refused reply was not logged: %s", logs.String())
	}
}

// The assistant is unnamed when the message's agent is gone or is not a
// configured agent.
func TestTurnReplyCov_AssistantName(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksCovLoop(t)
	gone := bus.InboundMessage{Channel: "telegram", ChatID: "42", Metadata: map[string]string{metadataKeyPreresolvedAgentID: "nobody"}}
	if got := al.turnAssistantName(gone); got != "" {
		t.Fatalf("name of a gone agent = %q, want none", got)
	}
	id, err := newAgentServices(al, "main").CreateFresh("test-model")
	if err != nil {
		t.Fatal(err)
	}
	temp := bus.InboundMessage{Channel: "telegram", ChatID: "42", Metadata: map[string]string{metadataKeyPreresolvedAgentID: id}}
	if got := al.turnAssistantName(temp); got != "" {
		t.Fatalf("name of a temporary agent = %q, want none", got)
	}
	if got := al.turnAssistantName(bus.InboundMessage{Channel: "telegram", ChatID: "42"}); got != "Main" {
		t.Fatalf("name of the default agent = %q, want Main", got)
	}
}

// Replies the bus refuses are logged: a turn's reply and a cancelled
// message's.
func TestTurnReplyCov_RepliesRefused(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksCovLoop(t)
	msgBus.Close()
	ctx := context.Background()
	msg := bus.InboundMessage{Channel: "telegram", ChatID: "42", MessageID: "m1"}
	al.publishInboundReply(ctx, ctx, msg, "hello", bus.OutcomeOK, false)
	required := msg
	required.Metadata = map[string]string{bus.MetaReplyRequired: "1"}
	al.publishCancelledReplies(ctx, []bus.InboundMessage{required}, errors.New("stopped"))
	for _, want := range []string{"Failed to publish outbound response", "Failed to publish cancelled reply"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("%q was not logged: %s", want, logs.String())
		}
	}
}

// A message with no reply of its own has its chat's indicators cleared.
func TestTurnReplyCov_DismissInbound(t *testing.T) {
	al := &AgentLoop{}
	d := &fakeDismisser{done: make(chan struct{}, 1)}
	al.dismisser = d
	al.dismissInbound(context.Background(), bus.InboundMessage{Channel: "telegram", ChatID: ""})
	al.dismissInbound(context.Background(), bus.InboundMessage{Channel: "telegram", ChatID: "42", MessageID: "m1"})
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen) != 1 || d.seen[0] != "telegram:42:m1" {
		t.Fatalf("dismissed = %v, want only telegram:42:m1", d.seen)
	}
}
