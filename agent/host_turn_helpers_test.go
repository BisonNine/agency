// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/providers"
)

// hostTurn is the engine turn the host builds for agent on opts, over
// messages, with tm as the turn's model choice (none when nil): the turn
// runLLMIteration runs, its callbacks registered. al defaults to a loop on an
// empty configuration.
func hostTurn(tb testing.TB, al *AgentLoop, agent *AgentInstance, opts processOptions, tm *turnModel, messages ...providers.Message) *engineTurn {
	tb.Helper()
	if al == nil {
		al = &AgentLoop{cfg: &config.Config{}}
	}
	if tm == nil {
		tm = &turnModel{}
	}
	opts.turnModel = tm
	turn, _ := al.newHostEngineTurn(context.Background(), agent, messages, opts, nil, nil)
	return turn
}

// newTestDispatch is the dispatch of turn's next request, with no tools.
func newTestDispatch(turn *engineTurn) *llmDispatch {
	return turn.newDispatch(context.Background(), nil, 0)
}
