// Agency
// License: MIT

package agent

import (
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// A binding naming an agent that is configured but not running routes to the
// default agent.
func TestRouting_NotRunningBoundAgentFallsBack(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t, func(cfg *config.Config) {
		off := false
		cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "ops", Enabled: &off})
		cfg.Bindings = []config.AgentBinding{{AgentID: "ops", Match: config.BindingMatch{Channel: "test"}}}
	})
	route, agent, err := al.resolveMessageRoute(inbound("c1", "m1", "hi"))
	if route.AgentID != "ops" {
		t.Fatalf("route = %+v, want the binding's agent", route)
	}
	if err != nil || agent == nil || agent.ID != "main" {
		t.Fatalf("resolveMessageRoute = %v, %v; want the default agent", agent, err)
	}
}

// Without a registry no mention is extracted.
func TestRouting_MentionWithoutRegistry(t *testing.T) {
	msg := bus.InboundMessage{Content: "@ops hi"}
	(&AgentLoop{}).extractMention(&msg)
	if msg.Content != "@ops hi" || msg.Metadata["mentioned_agent"] != "" {
		t.Fatalf("message = %+v, want it unchanged", msg)
	}
}

// A sender with a label and no id is archived by its label.
func TestRouting_SenderSourceLabelOnly(t *testing.T) {
	if got := senderSource("", bus.SenderInfo{DisplayName: "Bob"}); got != "Bob" {
		t.Fatalf("senderSource = %q, want Bob", got)
	}
}
