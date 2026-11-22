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
	// direct is the model whose client serves an agent none of whose own
	// models resolves (defaultModelClient); "" otherwise.
	direct string
}

// selectTurnModel makes the model choice for a turn of agent on sessionKey:
// the agent's chat models pinned for the turn, the session's /model choice
// first and the rest in their order, so a
// multi-step tool chain never switches model mid-way. A fresh temporary
// agent's CLI models run isolated in its own workspace.
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
	var model, direct string
	if len(candidates) > 0 {
		model = candidates[0].Name
	} else {
		// The name status and /model show too (activeModelName).
		model = al.activeModelName(agent, sessionKey)
		direct = al.defaultModelClient()
	}
	return &turnModel{chain: chain, candidates: candidates, model: model, limits: agent.limitsFor(model), direct: direct}
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
