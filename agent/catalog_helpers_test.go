// ClawEh
// License: MIT

package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// testProviderName is the provider withTestModels adds the models a test
// config names but does not define to.
const testProviderName = "test"

// withTestModels defines, on a "test" provider, every model cfg's agents,
// worker lists and summarization lists name that cfg does not define, so the
// catalogue has them; routeTestModels then points them at the test's mock.
// It returns their names.
func withTestModels(cfg *config.Config) []string {
	var names []string
	add := func(list []string) {
		for _, raw := range list {
			n := strings.TrimSpace(raw)
			if n == "" || slices.Contains(names, n) {
				continue
			}
			// Defined, enabled or not: the test means it as configured.
			if slices.ContainsFunc(cfg.Models, func(m config.ModelConfig) bool { return m.ModelName == n || m.Model == n }) {
				continue
			}
			names = append(names, n)
		}
	}
	add(cfg.Agents.Defaults.Models)
	add(cfg.Summarization.Models)
	for i := range cfg.Agents.List {
		ac := &cfg.Agents.List[i]
		add(ac.Models)
		add(ac.SummarizationModels)
		if ac.Subagents != nil {
			add(ac.Subagents.Models)
		}
	}
	// A provider a test model names but the test does not define.
	for i := range cfg.Models {
		p := cfg.Models[i].Provider
		if _, err := cfg.GetProvider(p); err != nil && p != "" {
			cfg.Providers = append(cfg.Providers, config.Provider{Name: p, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "test"})
		}
	}
	if len(names) == 0 {
		return nil
	}
	if _, err := cfg.GetProvider(testProviderName); err != nil {
		cfg.Providers = append(cfg.Providers, config.Provider{
			Name: testProviderName, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "test",
		})
	}
	for _, n := range names {
		cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: n, Model: n, Provider: testProviderName, Enabled: true, Vision: config.VisionOff})
	}
	return names
}

// routeTestModels registers, for each of names, a client that answers with
// the Provider of the agent the call is for (agentProviderClient), so a test
// that sets an agent's Provider drives its model calls.
func routeTestModels(al *AgentLoop, fallback providers.LLMProvider, names []string) {
	for _, n := range names {
		if err := al.catalog.RegisterClient(n, &agentProviderClient{al: al, fallback: fallback}); err != nil {
			panic(err)
		}
	}
}

// agentProviderClient is a model client that forwards each call to the
// Provider of the agent the call is billed to (a clone's to its source's),
// or to fallback for an agent the registry does not hold.
type agentProviderClient struct {
	al       *AgentLoop
	fallback providers.LLMProvider
}

func (c *agentProviderClient) target(id providers.Identity) providers.LLMProvider {
	if r := c.al.GetRegistry(); r != nil {
		// The agent whose identity it is (a clone has its own label), then
		// the agent it is billed to.
		for _, agentID := range r.All() {
			if inst, ok := r.Get(agentID); ok && inst.Identity() == id && inst.Provider != nil {
				return inst.Provider
			}
		}
		if inst, ok := r.Get(id.Agent); ok && inst != nil && inst.Provider != nil {
			return inst.Provider
		}
	}
	return c.fallback
}

func (c *agentProviderClient) Chat(ctx context.Context, id providers.Identity, messages []providers.Message, tools []providers.ToolDefinition, model string, opts map[string]any) (*providers.LLMResponse, error) {
	return c.target(id).Chat(ctx, id, messages, tools, model, opts)
}

func (c *agentProviderClient) GetDefaultModel() string { return "" }

// defaultTarget is the default agent's Provider: what the capability checks,
// which carry no identity, are answered from.
func (c *agentProviderClient) defaultTarget() providers.LLMProvider {
	if r := c.al.GetRegistry(); r != nil {
		if inst := r.Default(); inst != nil && inst.Provider != nil {
			return inst.Provider
		}
	}
	return c.fallback
}

// SupportsThinking is the default agent provider's.
func (c *agentProviderClient) SupportsThinking() bool {
	tc, ok := c.defaultTarget().(providers.ThinkingCapable)
	return ok && tc.SupportsThinking()
}

// IsCLI is the default agent provider's.
func (c *agentProviderClient) IsCLI() bool {
	p, ok := c.defaultTarget().(providers.CLIProvider)
	return ok && p.IsCLI()
}

// testCatalog builds the catalogue of cfg as the loop does, with client
// registered for every model named in clients.
func testCatalog(cfg *config.Config, clients map[string]providers.LLMProvider) *models.Catalog {
	c := providers.NewCatalogBuilder(nil, nil).Build(cfg)
	for name, cl := range clients {
		if err := c.RegisterClient(name, cl); err != nil {
			panic(err)
		}
	}
	return c
}

// catalogTurn is a turn of inst on the catalogue cfg builds (real clients),
// its chain pinned as a turn pins it.
func catalogTurn(t *testing.T, cfg *config.Config, inst *AgentInstance) *llmTurn {
	t.Helper()
	al := &AgentLoop{cfg: cfg, catalog: testCatalog(cfg, nil), activeModelIdx: map[string]int{}}
	assignAgentModels(al.catalog, inst)
	tm := al.selectTurnModel(inst, "s")
	if len(tm.candidates) == 0 {
		t.Fatal("the agent has no model in the catalogue")
	}
	return &llmTurn{al: al, agent: inst, chain: tm.chain, candidates: tm.candidates, model: tm.model, limits: tm.limits}
}

// parkFor parks model name in cat for d (a 429 with that Retry-After), as a
// turn's chain would, then gives the model client back.
func parkFor(t *testing.T, cat *models.Catalog, name string, d time.Duration, client providers.LLMProvider) {
	t.Helper()
	info, ok := cat.Get(name)
	if !ok {
		t.Fatalf("no model %s", name)
	}
	if err := cat.RegisterClient(name, modelstest.New().Script(info.Model.Model, modelstest.Status(429, d))); err != nil {
		t.Fatal(err)
	}
	// A Retry-After over 3 s is not waited out, so one failure parks it.
	if _, err := providers.ModelSet(cat, name).Begin(models.Chat).Chat(context.Background(), testIdentity, models.Request{}); err == nil {
		t.Fatalf("the failing call to %s succeeded", name)
	}
	if err := cat.RegisterClient(name, client); err != nil {
		t.Fatal(err)
	}
	if info, _ := cat.Get(name); info.Health.Available {
		t.Fatalf("%s is not parked", name)
	}
}

// routeAllModels points every model of the loop's catalogue at the Provider
// of the agent each call is for: a loop built without a dispatcher, whose
// tests drive the agents' Provider.
func routeAllModels(al *AgentLoop, fallback providers.LLMProvider) {
	routeTestModels(al, fallback, al.catalog.Names())
}

// adoptTestCatalog rebuilds the loop's catalogue from its config, which the
// test changed after building the loop, with every model routed to the
// agents' Provider (routeAllModels). Agents built before keep no models.
func adoptTestCatalog(al *AgentLoop, fallback providers.LLMProvider) {
	al.catalog.Adopt(al.catalogs.Build(al.GetConfig()))
	routeAllModels(al, fallback)
}

// reloadForTest is ReloadProviderAndConfig for a test: the models cfg names
// but does not define are defined (withTestModels), and afterwards every
// model is routed to the agents' Provider (routeAllModels), as at start.
func reloadForTest(al *AgentLoop, ctx context.Context, provider providers.LLMProvider, cfg *config.Config) error {
	if cfg != nil {
		withTestModels(cfg)
	}
	if err := al.ReloadProviderAndConfig(ctx, provider, cfg); err != nil {
		return err
	}
	routeAllModels(al, provider)
	return nil
}
