// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// mustNewAgentLoop builds an AgentLoop or fails the test.
func mustNewAgentLoop(
	t *testing.T,
	cfg *config.Config,
	msgBus *bus.MessageBus,
	provider providers.LLMProvider,
	clients clientRouting,
	opts ...LoopOption,
) *AgentLoop {
	t.Helper()
	added := withTestModels(cfg)
	al, err := NewAgentLoop(t.Context(), cfg, msgBus, opts...)
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	if clients == nil {
		// Without a dispatcher every model call went to the agent's
		// Provider; it still does.
		routeAllModels(al, provider)
	} else {
		routeTestModels(al, provider, added)
	}
	// Closing the registry removes a non-owner's private temp root.
	t.Cleanup(al.GetRegistry().Close)
	return al
}

// mustNewAgentRegistry builds a registry of bare instances (no tools, no
// loop) or fails the test.
func mustNewAgentRegistry(t *testing.T, cfg *config.Config, provider providers.LLMProvider) *AgentRegistry {
	t.Helper()
	registry, err := agentreg.New(t.Context(), cfg, agentreg.Hooks[*AgentInstance]{
		Build: func(_ context.Context, c *config.Config, spec agentreg.Spec) (*AgentInstance, error) {
			inst, err := newAgentInstance(spec, &c.Agents.Defaults, c)
			if err == nil {
				setTestProvider(inst, provider)
			}
			return inst, err
		},
	})
	if err != nil {
		t.Fatalf("agentreg.New: %v", err)
	}
	return registry
}

// mustNewAgentInstance builds an AgentInstance or fails the test.
func mustNewAgentInstance(
	t *testing.T,
	agentCfg *config.AgentConfig,
	defaults *config.AgentDefaults,
	cfg *config.Config,
	provider providers.LLMProvider,
) *AgentInstance {
	t.Helper()
	instance, err := NewAgentInstance(agentCfg, defaults, cfg)
	if err != nil {
		t.Fatalf("NewAgentInstance: %v", err)
	}
	setTestProvider(instance, provider)
	return instance
}
