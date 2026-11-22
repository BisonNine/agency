// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// catalogReloadConfig is a config for Alice on model alpha, in baseDir.
func catalogReloadConfig(baseDir, agentName string) *config.Config {
	return &config.Config{
		Agents: config.AgentsConfig{
			BaseDir:  baseDir,
			Defaults: config.AgentDefaults{Models: []string{"alpha"}, MaxTokens: 4096, MaxToolIterations: 10},
			List:     []config.AgentConfig{{ID: "alice", Name: agentName, Default: true}},
		},
		Providers: []config.Provider{{Name: "p", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}},
		Models:    []config.ModelConfig{{ModelName: "alpha", Model: "alpha-wire", Provider: "p", Enabled: true}},
	}
}

// A config save that does not touch models keeps the running catalogue, and
// with it the cooldowns; one that does hands the agents a new catalogue whose
// models start available.
func TestReload_CooldownsKeptUnlessModelsChange(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	model := &mockProvider{}
	al := mustNewAgentLoop(t, catalogReloadConfig(base, "Alice"), bus.NewMessageBus(), model, nil)
	client := &agentProviderClient{al: al, fallback: model}
	parkFor(t, al.catalog, "alpha", time.Minute, client)
	parked := func() bool {
		info, ok := al.catalog.Get("alpha")
		return ok && !info.Health.Available
	}

	// Renaming the agent changes nothing the catalogue is built from.
	if err := al.ReloadProviderAndConfig(context.Background(), model, catalogReloadConfig(base, "Alice Smith")); err != nil {
		t.Fatal(err)
	}
	if !parked() {
		t.Fatal("a save that does not touch models cleared the cooldown")
	}
	if a, _ := al.GetRegistry().Get("alice"); a.Name != "Alice Smith" {
		t.Fatalf("the reload did not apply: name %q", a.Name)
	}
	tm := al.selectTurnModel(al.GetRegistry().Default(), "s")
	if len(tm.candidates) != 1 || tm.candidates[0].Name != "alpha" {
		t.Fatalf("after the reload the agent's chain is %+v, want alpha", tm.candidates)
	}

	// A changed model setting is a new catalogue: cooldowns are cleared.
	changed := catalogReloadConfig(base, "Alice Smith")
	changed.Models[0].RequestTimeout = 90
	if err := al.ReloadProviderAndConfig(context.Background(), model, changed); err != nil {
		t.Fatal(err)
	}
	if parked() {
		t.Fatal("a save that changes models kept the cooldown")
	}
	if info, _ := al.catalog.Get("alpha"); info.Model.RequestTimeout != 90 {
		t.Fatalf("request timeout = %d, want the new 90", info.Model.RequestTimeout)
	}
	tm = al.selectTurnModel(al.GetRegistry().Default(), "s")
	if len(tm.candidates) != 1 || tm.candidates[0].Name != "alpha" {
		t.Fatalf("after adopting, the agent's chain is %+v, want alpha", tm.candidates)
	}
}

// The digest follows what the catalogue is built from, and nothing else.
func TestCatalogDigest(t *testing.T) {
	base := catalogReloadConfig("/x", "Alice")
	same := func(change func(*config.Config)) bool {
		c := catalogReloadConfig("/x", "Alice")
		change(c)
		return catalogDigest(c) == catalogDigest(base)
	}
	for name, tc := range map[string]struct {
		change func(*config.Config)
		same   bool
	}{
		"agent renamed":         {func(c *config.Config) { c.Agents.List[0].Name = "Bob" }, true},
		"channel setting":       {func(c *config.Config) { c.Agents.Defaults.MaxToolIterations = 3 }, true},
		"model setting":         {func(c *config.Config) { c.Models[0].ReasoningEffort = "high" }, false},
		"provider key":          {func(c *config.Config) { c.Providers[0].APIKey = "other" }, false},
		"agent models":          {func(c *config.Config) { c.Agents.List[0].Models = []string{"alpha"} }, false},
		"cooldown policy":       {func(c *config.Config) { c.Cooldown.RateLimitMinutes = 3 }, false},
		"log content":           {func(c *config.Config) { c.Logging.LogMessageContent = true }, false},
		"summarization models":  {func(c *config.Config) { c.Summarization.Models = []string{"alpha"} }, false},
		"agents' request limit": {func(c *config.Config) { c.Agents.Defaults.RequestTimeout = 30 }, false},
		"default context":       {func(c *config.Config) { c.Agents.Defaults.ContextWindow = 1000 }, false},
	} {
		if got := same(tc.change); got != tc.same {
			t.Errorf("%s: digest unchanged = %v, want %v", name, got, tc.same)
		}
	}
}

// deadlineProvider holds the first model until the call's context ends.
type deadlineProvider struct{ replayProvider }

func (p *deadlineProvider) Chat(ctx context.Context, id providers.Identity, m []providers.Message, tl []providers.ToolDefinition, model string, o map[string]any) (*providers.LLMResponse, error) {
	if model == "gpt-4" {
		p.mu.Lock()
		p.called = append(p.called, model)
		p.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.replayProvider.Chat(ctx, id, m, tl, model, o)
}

// When the turn's deadline passes during a model call, the chain stops: the
// next model is not called on the dead context, no fallback is announced or
// logged, and the turn's reply is the time-limit message.
func TestCallModel_TurnDeadlineStopsTheChain(t *testing.T) {
	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	p := &deadlineProvider{}
	turn, d := chainTurn(t, p, nil, replayGPT, replayClaude)
	var notices int
	turn.notifier = func([]providers.FallbackAttempt, providers.FallbackCandidate) { notices++ }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := turn.requestModel(ctx, d)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if got := p.calls(); !slices.Equal(got, []string{"gpt-4"}) {
		t.Fatalf("calls = %v, want only the model the deadline cut short", got)
	}
	if notices != 0 {
		t.Fatalf("%d fallback notices, want none", notices)
	}
	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish)
	if got := renderTurnError(ctx, 15*time.Minute, err); !strings.HasPrefix(got, "⚠️ This turn ran past the 15m time limit") {
		t.Fatalf("reply = %q, want the time-limit message", got)
	}
}
