// ClawEh
// License: MIT

package agent

import (
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
)

// The facts the turn engine reads from a model candidate (Limits,
// ProviderSpec) are those the configuration gave the turn before: the
// agent's limits for the model (agent.limitsFor) and the model's vision
// and its provider's require_reasoning_content (modelFacts), for models
// with and without limits of their own, every vision mode and thinking
// level, and agents with their own settings.
func TestModelFacts_CandidatesMatchTheConfiguration(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.ContextWindow = 64000
	cfg.Providers = append(cfg.Providers,
		config.Provider{Name: "Bob's API", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k", RequireReasoningContent: true})
	cfg.Models = append(cfg.Models,
		config.ModelConfig{
			ModelName: "Alice Model", Model: "alice-wire", Provider: testProviderName, Enabled: true,
			MaxTokens: 32768, ContextWindow: 200000, ThinkingLevel: "high", Vision: config.VisionUserMessage,
		},
		config.ModelConfig{ModelName: "Bob Model", Model: "bob-wire", Provider: "Bob's API", Enabled: true},
		config.ModelConfig{
			ModelName: "Carol Model", Model: "carol-wire", Provider: testProviderName, Enabled: true,
			ThinkingLevel: "off", NoTools: true, Vision: config.VisionToolResponse,
		},
		config.ModelConfig{
			ModelName: "Dave Model", Model: "dave-wire", Provider: "Bob's API", Enabled: true,
			MaxTokens: 600, ThinkingLevel: "adaptive", Vision: config.VisionOff,
		})
	iterations, temperature := 7, 0.3
	cfg.Agents.List = append(cfg.Agents.List,
		config.AgentConfig{ID: "alice", Name: "Alice", Models: []string{"Alice Model", "Bob Model", "Carol Model", "Dave Model"}},
		config.AgentConfig{
			ID: "bob", Name: "Bob", Models: []string{"Dave Model", "test-model"},
			MaxToolIterations: &iterations, Temperature: &temperature,
		})
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)

	for id, want := range map[string]int{"alice": 4, "bob": 2} {
		agent, ok := al.GetRegistry().Get(id)
		if !ok {
			t.Fatalf("no agent %s", id)
		}
		tm := al.selectTurnModel(agent, "s")
		if len(tm.candidates) != want {
			t.Fatalf("%s: %d candidates, want %d", id, len(tm.candidates), want)
		}
		for _, c := range tm.candidates {
			l := agent.limitsFor(c.Name)
			facts := al.modelFacts(agent, c.Name)
			vision := facts.Limits.Vision
			if vision == "" {
				vision = modelspec.VisionOff // an unset mode has always read as off
			}
			if c.Limits.MaxTokens != l.maxTokens || c.Limits.ContextWindow != l.contextWindow ||
				c.Limits.ThinkingLevel != l.thinkingLevel || c.Limits.NoTools != l.noTools ||
				c.Limits.Vision != vision ||
				c.ProviderSpec.RequireReasoningContent != facts.ProviderSpec.RequireReasoningContent {
				t.Errorf("%s / %s: candidate %+v, require %v; configuration %+v, vision %q, require %v",
					id, c.Name, c.Limits, c.ProviderSpec.RequireReasoningContent, l, vision, facts.ProviderSpec.RequireReasoningContent)
			}
		}
	}
}
