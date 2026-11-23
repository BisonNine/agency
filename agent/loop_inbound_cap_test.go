// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/loop"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/providers"
)

// An async sub-agent result arrives later as a "system" message and is spliced
// into a fresh turn, so it must be capped like a synchronous tool result: an
// oversized one otherwise fails the turn it lands in. The cap is the owning
// agent's, and the head of the result plus the truncation marker reach the LLM.
func TestProcessSystemMessage_CapsAsyncResult(t *testing.T) {
	al := newTestAgentLoop(t).al
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	agent.ContextWindow = 8000 // cap floors at 16 KiB
	agent.modelLimits = nil    // the agent's own window, whatever model serves
	sp := &sequenceProvider{
		responses: []*providers.LLMResponse{{Content: "noted", FinishReason: "stop"}},
		errors:    []error{nil},
	}
	setTestProvider(agent, sp)

	big := strings.Repeat("r", 100_000)
	_, err := al.processSystemMessage(context.Background(), bus.InboundMessage{
		Channel:  "system",
		SenderID: "async:agent_spawn",
		ChatID:   "chat-1",
		Content:  big,
		Metadata: map[string]string{bus.MetaOriginChannel: "telegram"},
	}, nil)
	if err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	var got string
	for _, m := range sp.lastMessages {
		if strings.HasPrefix(m.Content, "[System: async:agent_spawn]") {
			got = m.Content
		}
	}
	if got == "" {
		t.Fatalf("system message did not reach the LLM: %+v", sp.lastMessages)
	}
	if !strings.Contains(got, "[output truncated: kept ") {
		t.Fatal("oversized async result reached the LLM uncapped")
	}
	if want := loop.ToolResultCap(agent.ContextWindow); len(got) > want+512 {
		t.Fatalf("capped message is %d chars, cap is %d", len(got), want)
	}
	if strings.Count(got, "r") < loop.ToolResultCap(0)/2 {
		t.Fatal("cap dropped the head of the result rather than the tail")
	}
}
