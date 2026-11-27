// Agency - Cognitive Memory
// License: MIT

package agent

import (
	"context"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/global"
)

// chainCompleter is the plain-typed chain walk the host's summarization
// caller exposes (compressModelCaller.complete), so the same walk can serve a
// second caller shape without either module importing the other.
type chainCompleter func(ctx context.Context, system, user string, jsonObject bool, exclude []string) (content, finishReason, model string, err error)

// memoryModelCaller adapts the agent's summarization model chain to
// hook.Caller for consolidation. Consolidation reuses the same chain as context
// compaction — there is no separate memory-model config (see
// config.MemoryConfig) — and the same host client: one chain walk, honouring
// Exclude and the catalogue's cooldowns, serving both modules. cogmem
// validates the reply itself and calls again with a longer Exclude when a
// model returns something unusable.
type memoryModelCaller struct {
	complete  chainCompleter
	modelName string // first resolved model name, for run records
}

// ModelName returns the human-readable name of the first model in the chain.
func (c *memoryModelCaller) ModelName() string { return c.modelName }

// Complete implements hook.Caller.
func (c *memoryModelCaller) Complete(ctx context.Context, req hook.CompleteRequest) (hook.CompleteReply, error) {
	content, finishReason, model, err := c.complete(ctx, req.System, req.User, req.JSONObject, req.Exclude)
	return hook.CompleteReply{Content: content, FinishReason: finishReason, Model: model}, err
}

// NewMemoryModelCaller builds a hook.Caller for the given agent
// over the agent's summarization model chain (agent.summarization_models →
// global summarization.models → the agent's primary chat model last),
// on the agent's set for models.Memory, the same list as context compaction.
// This is the constructor the gateway's WorkerFactory calls per Job.
func (al *AgentLoop) NewMemoryModelCaller(agent *AgentInstance) hook.Caller {
	caller, effective := al.newCompressModelCaller(agent, "cogmem", models.Memory)
	// "cogmem" labels the chain's own log lines; it is not a session key.
	caller.noSessionKey = true
	// Consolidation reads several sessions' messages, so it is billed to a
	// code of its own, never to one of theirs.
	caller.billing = func(context.Context) string { return global.MemoryBilling }
	// It runs for the agent, with nobody above it: the agent is its origin.
	caller.ownOrigin = true
	return &memoryModelCaller{complete: caller.complete, modelName: effective}
}
