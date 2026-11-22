// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm/openai_compat"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// scriptedProvider answers Chat with a canned response or error and records
// every call's model and options, so a test can see the chain walk.
type scriptedProvider struct {
	mu      sync.Mutex
	content string
	err     error
	calls   []scriptedCall
}

type scriptedCall struct {
	model string
	opts  map[string]any
}

func (p *scriptedProvider) Chat(_ context.Context, _ providers.Identity, _ []providers.Message, _ []providers.ToolDefinition, model string, opts map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, scriptedCall{model: model, opts: opts})
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return &providers.LLMResponse{Content: p.content, FinishReason: "stop"}, nil
}

func (p *scriptedProvider) GetDefaultModel() string { return "scripted" }

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// compressCatalog is a catalogue with one model per client, each on its own
// provider and named as its wire id, answered by that client.
func compressCatalog(clients map[string]providers.LLMProvider) *models.Catalog {
	cfg := &config.Config{}
	for name := range clients {
		cfg.Providers = append(cfg.Providers, config.Provider{Name: "p-" + name, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"})
		cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: name, Model: name, Provider: "p-" + name, Enabled: true})
	}
	return testCatalog(cfg, clients)
}

// compressClient is the summarization client of model on cat.
func compressClient(cat *models.Catalog, model string) *providerLLMClient {
	return &providerLLMClient{catalog: cat, alias: model, model: model, ourProvider: "p-" + model}
}

// TestCompressModelCaller_WalksChainAndSkipsExcluded: the first model not in
// Exclude answers, the reply names it, and excluded models are never called.
func TestCompressModelCaller_WalksChainAndSkipsExcluded(t *testing.T) {
	first := &scriptedProvider{content: "from-first"}
	second := &scriptedProvider{content: "from-second"}
	cat := compressCatalog(map[string]providers.LLMProvider{"model-a": first, "model-b": second})
	c := &compressModelCaller{id: testIdentity, clients: []*providerLLMClient{
		compressClient(cat, "model-a"),
		compressClient(cat, "model-b"),
	}}

	reply, err := c.Complete(context.Background(), ctxengine.ModelRequest{System: "s", User: "u", JSONObject: true})
	if err != nil || reply.Content != "from-first" || reply.Model != "model-a" {
		t.Fatalf("Complete = %+v, %v; want model-a's reply", reply, err)
	}

	reply, err = c.Complete(context.Background(), ctxengine.ModelRequest{System: "s", User: "u", Exclude: []string{"model-a"}})
	if err != nil || reply.Content != "from-second" || reply.Model != "model-b" {
		t.Fatalf("Complete with model-a excluded = %+v, %v; want model-b's reply", reply, err)
	}
	if first.callCount() != 1 {
		t.Fatalf("excluded model was called: %d calls", first.callCount())
	}

	_, err = c.Complete(context.Background(), ctxengine.ModelRequest{Exclude: []string{"model-a", "model-b"}})
	if !errors.Is(err, ctxengine.ErrNoModel) || !strings.Contains(err.Error(), "2 excluded") {
		t.Fatalf("every model excluded should yield ErrNoModel with the count, got %v", err)
	}
}

// TestCompressModelCaller_ErrorMovesOnAndNamesLastModel: a transport error on
// one model moves to the next; when every model fails the last error comes
// back, as the client returned it, together with the last model tried, so
// the report can name it.
func TestCompressModelCaller_ErrorMovesOnAndNamesLastModel(t *testing.T) {
	broken := &scriptedProvider{err: errors.New("boom")}
	good := &scriptedProvider{content: "ok"}
	cat := compressCatalog(map[string]providers.LLMProvider{"model-a": broken, "model-b": good})
	c := &compressModelCaller{id: testIdentity, clients: []*providerLLMClient{
		compressClient(cat, "model-a"),
		compressClient(cat, "model-b"),
	}}
	reply, err := c.Complete(context.Background(), ctxengine.ModelRequest{})
	if err != nil || reply.Model != "model-b" {
		t.Fatalf("expected the chain to move past the failing model: %+v, %v", reply, err)
	}

	onlyBroken := &compressModelCaller{id: testIdentity, clients: []*providerLLMClient{compressClient(cat, "model-a")}}
	reply, err = onlyBroken.Complete(context.Background(), ctxengine.ModelRequest{})
	if err == nil || err.Error() != "boom" || reply.Model != "model-a" {
		t.Fatalf("expected the last error with the model named: %+v, %v", reply, err)
	}
}

// TestCompressModelCaller_JSONObjectForwarded: the engine's JSONObject flag
// becomes the provider's response-format option, and its absence sends none.
func TestCompressModelCaller_JSONObjectForwarded(t *testing.T) {
	p := &scriptedProvider{content: "{}"}
	cat := compressCatalog(map[string]providers.LLMProvider{"m": p})
	c := &compressModelCaller{id: testIdentity, clients: []*providerLLMClient{compressClient(cat, "m")}}
	if _, err := c.Complete(context.Background(), ctxengine.ModelRequest{JSONObject: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), ctxengine.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if v, ok := p.calls[0].opts[openai_compat.ResponseFormatJSONObjectOption].(bool); !ok || !v {
		t.Errorf("JSONObject request did not set the response-format option: %v", p.calls[0].opts)
	}
	if p.calls[1].opts != nil {
		t.Errorf("plain request should carry no options, got %v", p.calls[1].opts)
	}
}

// parkModel parks model on cat with a billing failure, as a turn's chain
// would (the health is the catalogue's, shared by every path).
func parkModel(t *testing.T, cat *models.Catalog, model string) {
	t.Helper()
	failing := &scriptedProvider{err: errors.New("API request failed:   Status: 402   Body: {}")}
	if err := cat.RegisterClient(model, failing); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.ModelSet(cat, model).Begin(models.Chat).Chat(context.Background(), testIdentity, models.Request{}); err == nil {
		t.Fatal("the failing call succeeded")
	}
	if info, _ := cat.Get(model); info.Health.Available {
		t.Fatalf("%s should be parked", model)
	}
}

// TestCompressModelCaller_SharedCooldownSkipsModel: a model parked by another
// path (e.g. an out-of-credits 402 hit by a turn's chain) is skipped by the
// compaction path — not retried — and the error says so.
func TestCompressModelCaller_SharedCooldownSkipsModel(t *testing.T) {
	p := &scriptedProvider{content: "{}"}
	cat := compressCatalog(map[string]providers.LLMProvider{"abliterated-model": p})
	parkModel(t, cat, "abliterated-model")
	if err := cat.RegisterClient("abliterated-model", p); err != nil {
		t.Fatal(err)
	}

	c := &compressModelCaller{clients: []*providerLLMClient{compressClient(cat, "abliterated-model")}, id: testIdentity}
	_, err := c.Complete(context.Background(), ctxengine.ModelRequest{})
	if !errors.Is(err, ctxengine.ErrNoModel) || !strings.Contains(err.Error(), "1 in cooldown") {
		t.Fatalf("cooled model should be skipped with ErrNoModel naming the cooldown, got %v", err)
	}
	if p.callCount() != 0 {
		t.Fatalf("model parked in the catalogue must not be called; calls=%d", p.callCount())
	}
}

// TestCompressModelCaller_BillingFailurePutsModelInCooldown: a summarization
// model returning a billing (402) error is put in cooldown and not retried on
// the next call — so an out-of-credits summarizer is not hammered.
func TestCompressModelCaller_BillingFailurePutsModelInCooldown(t *testing.T) {
	p := &scriptedProvider{err: errors.New("API request failed:   Status: 402   Body: {\"error\":{\"billing_url\":\"x\"}}")}
	cat := compressCatalog(map[string]providers.LLMProvider{"abliterated-model": p})
	c := &compressModelCaller{clients: []*providerLLMClient{compressClient(cat, "abliterated-model")}, id: testIdentity}

	reply, err := c.Complete(context.Background(), ctxengine.ModelRequest{})
	if err == nil || reply.Model != "abliterated-model" || p.callCount() != 1 {
		t.Fatalf("first call: reply=%+v err=%v calls=%d", reply, err, p.callCount())
	}
	if info, _ := cat.Get("abliterated-model"); info.Health.Available {
		t.Fatal("402 should have parked the model")
	}

	_, err = c.Complete(context.Background(), ctxengine.ModelRequest{})
	if !errors.Is(err, ctxengine.ErrNoModel) || p.callCount() != 1 {
		t.Fatalf("second call should skip the cooled model without dispatching: err=%v calls=%d", err, p.callCount())
	}
}

// TestSessionTokenLayer pins the token section's placement and text, and that
// an empty token yields an empty (skipped) layer.
func TestSessionTokenLayer(t *testing.T) {
	l := sessionTokenLayer("SST-abc")
	if l.Name != "session_token" || !l.AfterSummary {
		t.Fatalf("layer = %+v; want session_token after the summary", l)
	}
	if !strings.HasPrefix(l.Text, "# Session Token\n\n") || !strings.HasSuffix(l.Text, "session_token: SST-abc") {
		t.Fatalf("token layer text = %q", l.Text)
	}
	if empty := sessionTokenLayer(""); empty.Text != "" || !empty.AfterSummary {
		t.Fatalf("empty token should give an empty after-summary layer: %+v", empty)
	}
}

// issuingSTI hands out a new token on every Issue call.
type issuingSTI struct {
	recordingSTI
	n int
}

func (s *issuingSTI) Issue(_, _, _ string) string {
	s.n++
	return "SST-" + strings.Repeat("x", s.n)
}

// TestSessionToken_IssuedAndReissuedOnEntry: the token issued when a session's
// context manager is created lands in the dispatch layers, and a reissue
// (session clear) replaces it in place.
func TestSessionToken_IssuedAndReissuedOnEntry(t *testing.T) {
	al := newTestAgentLoop(t).al
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	al.SetSessionTokenIssuer(&issuingSTI{})

	const key = "token-entry"
	_, release := al.getContextManager(agent, key)
	defer release()

	opts := processOptions{SessionKey: key, Channel: "cli", ChatID: "direct"}
	layers := al.promptLayers(agent, opts)
	last := layers[len(layers)-1]
	if last.Name != "session_token" || !strings.HasSuffix(last.Text, "session_token: SST-x") {
		t.Fatalf("first dispatch token layer = %+v", last)
	}

	al.reissueSessionToken(agent, key)
	layers = al.promptLayers(agent, opts)
	last = layers[len(layers)-1]
	if !strings.HasSuffix(last.Text, "session_token: SST-xx") {
		t.Fatalf("reissued token not rendered: %+v", last)
	}
	if al.sessionToken(agent, "no-such-session") != "" {
		t.Fatal("an uncached session has no token")
	}
}
