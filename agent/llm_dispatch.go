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
// agent's CLI models run isolated in its own workspace. An agent with no
// chat model (none of its models is in the model list and enabled) gets a
// chain without candidates, and its turn fails with errNoModelConfigured:
// it never runs on another model.
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
	}
	return &turnModel{chain: chain, candidates: candidates, model: model, limits: agent.limitsFor(model)}
}
