// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agent/ctxengine"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// TestResolveCompressModelTarget covers the alias / shorthand / fully-qualified
// resolution paths that compress_model strings can take.
func TestResolveCompressModelTarget(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com/v1", APIKey: "k"},
			{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1", APIKey: "k"},
		},
		Models: []config.ModelConfig{
			{
				ModelName: "haiku",
				Model:     "claude-haiku-4-5",
				Provider:  "anthropic",
				Enabled:   true,
			},
			{
				ModelName: "fast",
				Model:     "gpt-4o-mini",
				Provider:  "openai",
				Enabled:   true,
			},
			{
				ModelName: "disabled",
				Model:     "gpt-3.5-turbo",
				Provider:  "openai",
				Enabled:   false,
			},
		},
	}

	cases := []struct {
		name      string
		raw       string
		wantAlias string
		wantModel string
		wantOK    bool
	}{
		{"alias_lookup_by_model_name", "haiku", "haiku", "claude-haiku-4-5", true},
		{"alias_lookup_by_model_id", "gpt-4o-mini", "fast", "gpt-4o-mini", true},
		{"raw_model_id_match", "claude-haiku-4-5", "haiku", "claude-haiku-4-5", true},
		{"unknown_bare_returns_not_found", "nope-no-match", "", "", false},
		{"disabled_model_id_is_ignored", "gpt-3.5-turbo", "", "", false},
		{"empty_string_returns_not_found", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, m, ok := resolveCompressModelTarget(cfg, tc.raw)
			if ok != tc.wantOK || a != tc.wantAlias || m != tc.wantModel {
				t.Fatalf("resolveCompressModelTarget(%q) = (%q,%q,%v); want (%q,%q,%v)",
					tc.raw, a, m, ok, tc.wantAlias, tc.wantModel, tc.wantOK)
			}
		})
	}
}

// TestResolveCompressModelTarget_NilConfig defends against the unlikely case
// where the loop has no config snapshot yet.
func TestResolveCompressModelTarget_NilConfig(t *testing.T) {
	_, _, ok := resolveCompressModelTarget(nil, "haiku")
	if ok {
		t.Fatalf("expected not-found on nil cfg")
	}
}

// compressTestConfig has one model, "compress-target" (wire id
// "test-compress-model"), and one, "primary-target" ("some-model").
func compressTestConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "dummy"},
		},
		Models: []config.ModelConfig{
			{ModelName: "compress-target", Model: "test-compress-model", Provider: "openai", Enabled: true},
			{ModelName: "primary-target", Model: "some-model", Provider: "openai", Enabled: true},
		},
	}
}

// TestBuildCompressLLMClient_ResolvesThroughCatalogue: a summarization model
// named by its wire id resolves to its models entry and is called on that
// model's chain in the catalogue, never through the agent's provider.
func TestBuildCompressLLMClient_ResolvesThroughCatalogue(t *testing.T) {
	cfg := compressTestConfig()
	target := &scriptedProvider{content: "summary"}
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, map[string]providers.LLMProvider{"compress-target": target})}
	agent := &AgentInstance{ID: "compress-test", Provider: &mockProvider{}, Model: "sonnet-4-5"}

	client := al.resolveCompressClient(agent, "test-compress-model", "sess-1")
	if client == nil || client.alias != "compress-target" || client.model != "test-compress-model" || client.ourProvider != "openai" {
		t.Fatalf("client = %+v, want compress-target / test-compress-model on openai", client)
	}
	c := &compressModelCaller{id: testIdentity, clients: []*providerLLMClient{client}}
	reply, err := c.Complete(context.Background(), ctxengine.ModelRequest{System: "s", User: "u"})
	if err != nil || reply.Content != "summary" || target.callCount() != 1 {
		t.Fatalf("Complete = %+v, %v (calls %d), want the catalogue's client to answer", reply, err, target.callCount())
	}
}

// TestBuildCompressLLMClient_UnknownModelSkipped: a summarization model that
// names no enabled model is left out of the chain.
func TestBuildCompressLLMClient_UnknownModelSkipped(t *testing.T) {
	cfg := &config.Config{}
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "fallback-test", Provider: &mockProvider{}}
	if client := al.resolveCompressClient(agent, "unknown-model", "sess-2"); client != nil {
		t.Fatalf("client = %+v, want none for a model that is not configured", client)
	}
}

// TestBuildDefaultCompressLLMClient_UsesCatalogueForPrimary: the agent's
// primary model, the walk's last resort, is called on its own chain in the
// catalogue, not through the agent's provider.
func TestBuildDefaultCompressLLMClient_UsesCatalogueForPrimary(t *testing.T) {
	cfg := compressTestConfig()
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "default-compress-test", Provider: &mockProvider{}, Model: "some-model"}

	client := al.resolveDefaultCompressClient(agent, "sess-default")
	if client == nil || client.alias != "primary-target" || client.model != "some-model" {
		t.Fatalf("client = %+v, want primary-target / some-model", client)
	}
}

// TestBuildDefaultCompressLLMClient_UnresolvedPrimarySkipped: a primary model
// that names no enabled model adds nothing to the chain.
func TestBuildDefaultCompressLLMClient_UnresolvedPrimarySkipped(t *testing.T) {
	cfg := &config.Config{}
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "default-fallback-test", Provider: &mockProvider{}, Model: "no-such-model"}
	if client := al.resolveDefaultCompressClient(agent, "sess-default-fallback"); client != nil {
		t.Fatalf("client = %+v, want none for an unresolved primary", client)
	}
}

// TestBuildCompressLLMClient_FreshAgentIsolated: a fresh temporary agent's
// summarization models run isolated in its own workspace.
func TestBuildCompressLLMClient_FreshAgentIsolated(t *testing.T) {
	cfg := compressTestConfig()
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "fresh", Workspace: "/tmp/fresh-ws"}
	agent.Spec.Fresh = true
	if client := al.resolveCompressClient(agent, "compress-target", "s"); client == nil || client.workspace != "/tmp/fresh-ws" {
		t.Fatalf("client = %+v, want it isolated in the agent's workspace", client)
	}
}
