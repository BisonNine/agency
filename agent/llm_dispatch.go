// ClawEh
// License: MIT

package agent

import (
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/providers"
)

// turnModel is a turn's model choice, made once when the turn starts so its
// assembly, tools, context window and requests all follow the same model:
// the chain pinned for the turn (the active model first), its candidates,
// the first one's model_name, and its limits.
type turnModel struct {
	chain      *models.Chain
	candidates []providers.FallbackCandidate
	model      string
	limits     runLimits
}

// selectTurnModel makes the model choice for a turn of agent on sessionKey:
// the agent's chat models pinned for the turn, the session's /model choice
// first and the rest in their order, so a
// multi-step tool chain never switches model mid-way. A fresh temporary
// agent's CLI models run isolated in its own workspace. An agent none of
// whose models resolves runs on the default model's client (directChain).
func (al *AgentLoop) selectTurnModel(agent *AgentInstance, sessionKey string) *turnModel {
	// Not while a reload's lists are being stated (listsGate); the chain
	// is pinned here, so the model call runs without the gate.
	al.listsGate.RLock()
	defer al.listsGate.RUnlock()
	set := al.catalog.Set(agent.ID)
	scope := models.Scope(sessionKey)
	chain := set.Begin(models.Chat, scope)
	if agent.Spec.Fresh {
		chain = chain.Isolated(agent.Workspace)
	}
	candidates := chain.Candidates()
	var model string
	if len(candidates) > 0 {
		model = candidates[0].Name
	} else {
		// The name status and /model show too (activeModelName).
		model = al.activeModelName(agent, sessionKey)
		if direct := al.directChain(agent, model); direct != nil {
			chain, candidates = direct, direct.Candidates()
		}
	}
	return &turnModel{chain: chain, candidates: candidates, model: model, limits: agent.limitsFor(model)}
}

// directChain is the chain of a turn of agent none of whose models resolves:
// one call on the default model's client (defaultModelClient) with model, the
// agent's first configured name, as the wire id (an empty one included: the
// client decides), reporting no provider, the agent's own limits for model
// and its provider's reasoning-content setting; nil when there is no default
// model, and the turn fails as a configuration without models.
func (al *AgentLoop) directChain(agent *AgentInstance, model string) *models.Chain {
	name := al.defaultModelClient()
	if name == "" {
		return nil
	}
	facts := al.modelFacts(agent, model)
	l := agent.limitsFor(model)
	limits := facts.Limits
	limits.ContextWindow, limits.NoTools = l.contextWindow, l.noTools
	chain, err := al.catalog.Direct(name, model,
		models.WithDirectProvider(""),
		models.WithDirectLimits(limits),
		models.WithDirectRequireReasoningContent(facts.ProviderSpec.RequireReasoningContent))
	if err != nil {
		return nil
	}
	return chain
}

// defaultModelClient is the model an agent none of whose models resolves
// runs on: the agents' default model (agents.defaults.models[0], by
// model_name), when the catalogue has it. Its client is called with the
// agent's first configured name as the model, with no fallback and no
// health kept, as the agent's own provider always served such an agent.
// "" when there is none: the turn fails as a configuration without models.
func (al *AgentLoop) defaultModelClient() string {
	cfg := al.GetConfig()
	if cfg == nil {
		return ""
	}
	mc, err := cfg.GetModelConfig(cfg.Agents.Defaults.DefaultModelName())
	if err != nil || mc == nil {
		return ""
	}
	if _, ok := al.catalog.Get(mc.ModelName); !ok {
		return ""
	}
	return mc.ModelName
}
