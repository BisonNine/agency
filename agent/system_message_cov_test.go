// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/logger"
)

// A system message that names no origin channel is dropped.
func TestSystemMessageCov_NoOriginDropped(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksCovLoop(t)
	resp, err := al.processSystemMessage(context.Background(), bus.InboundMessage{Channel: "system", SenderID: "subagent:1", ChatID: "42", Content: "done"}, nil)
	if resp != "" || err != nil {
		t.Fatalf("processSystemMessage = %q, %v; want it dropped", resp, err)
	}
	if !strings.Contains(logs.String(), "System message dropped: it names no origin channel") {
		t.Fatalf("the drop was not logged: %s", logs.String())
	}
	noOutbound(t, msgBus)
}

// A completion message without a result part is its own result.
func TestSystemMessageCov_SubagentResultWhole(t *testing.T) {
	if got := subagentResult("all done"); got != "all done" {
		t.Fatalf("subagentResult = %q", got)
	}
}

// A fallback chat the bus refuses is logged.
func TestSystemMessageCov_FallbackPublishFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksCovLoop(t)
	msgBus.Close()
	msg := bus.InboundMessage{Channel: "system", SenderID: "forum", Metadata: map[string]string{
		metadataKeyFallbackChannel: "telegram", metadataKeyFallbackChatID: "7",
	}}
	onDelivery := al.systemReplyFallback(context.Background(), msg, "main", "telegram", "42")
	onDelivery("the reply", channels.ErrRecipientOffline)
	waitFor(t, "the refused fallback to be logged", func() bool {
		return strings.Contains(logs.String(), "the fallback chat could not be queued either")
	})
}
