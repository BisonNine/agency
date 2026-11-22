// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/models"

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

// TestCompressCaller_ResolvesThroughCatalogue: a summarization model named by
// its wire id resolves to its models entry, is in the agent's Summarize list
// and is called through the catalogue, never anything else.
func TestCompressCaller_ResolvesThroughCatalogue(t *testing.T) {
	cfg := compressTestConfig()
	cfg.Summarization.Models = []string{"test-compress-model"}
	target := &scriptedProvider{content: "summary"}
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, map[string]providers.LLMProvider{"compress-target": target})}
	agent := &AgentInstance{ID: "compress-test", Model: "sonnet-4-5"}
	assignAgentModels(al.catalog, cfg, agent)

	c, effective := al.newCompressModelCaller(agent, "sess-1", models.Summarize)
	reply, err := c.Complete(context.Background(), ctxengine.ModelRequest{System: "s", User: "u"})
	if err != nil || reply.Content != "summary" || reply.Model != "test-compress-model" || target.callCount() != 1 {
		t.Fatalf("Complete = %+v, %v (calls %d), want the catalogue's client to answer", reply, err, target.callCount())
	}
	if effective != "test-compress-model" {
		t.Errorf("effective = %q, want the configured name", effective)
	}
}

// An unresolved summarization model and an unresolved primary are left out
// of the chain; with nothing left, the call is ErrNoModel.
func TestCompressCaller_UnresolvedModelsLeftOut(t *testing.T) {
	cfg := compressTestConfig()
	cfg.Summarization.Models = []string{"unknown-model"}
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "fallback-test", Model: "no-such-model"}
	assignAgentModels(al.catalog, cfg, agent)
	if got := al.catalog.Set(agent.ID).List(models.Summarize); len(got) != 0 {
		t.Fatalf("summarize list = %+v, want none", got)
	}
	c, _ := al.newCompressModelCaller(agent, "s", models.Summarize)
	_, err := c.Complete(context.Background(), ctxengine.ModelRequest{})
	if !errors.Is(err, ctxengine.ErrNoModel) || err.Error() != "no summarization model available: 0 excluded, 0 in cooldown" {
		t.Fatalf("err = %v, want ErrNoModel with today's text", err)
	}

	// With a resolvable primary it is the chain's last resort.
	agent = &AgentInstance{ID: "primary-test", Model: "some-model"}
	assignAgentModels(al.catalog, cfg, agent)
	if got := entryNames(al.catalog.Set(agent.ID).List(models.Summarize)); !slices.Equal(got, []string{"primary-target"}) {
		t.Fatalf("summarize list = %v, want the primary", got)
	}
}

// A fresh temporary agent's summarization models run isolated in its own
// workspace.
func TestCompressCaller_FreshAgentIsolated(t *testing.T) {
	cfg := compressTestConfig()
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil)}
	agent := &AgentInstance{ID: "fresh", Workspace: "/tmp/fresh-ws"}
	agent.Spec.Fresh = true
	if c, _ := al.newCompressModelCaller(agent, "s", models.Summarize); c.workspace != "/tmp/fresh-ws" {
		t.Fatalf("workspace = %q, want the agent's", c.workspace)
	}
}
