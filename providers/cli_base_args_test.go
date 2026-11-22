// ClawEh
// License: MIT

package providers

import (
	"slices"
	"testing"

	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
)

// The WebUI shows an operator the command line of each CLI from the
// catalogue (config.CLIAgents, modelspec's), while spawnllm builds the real
// invocation from its own base arguments. The two must agree, or the page
// shows a command that is not the one being run.
func TestCLIAgents_BaseArgsMatchTheInvocation(t *testing.T) {
	built := map[string][]string{
		"claude-cli":      spawnllm.ClaudeCliBaseArgs(),
		"codex-cli":       spawnllm.CodexCliBaseArgs(),
		"antigravity-cli": spawnllm.AntigravityCliBaseArgs(),
		"cursor-cli":      spawnllm.CursorCliBaseArgs(),
	}
	if len(config.CLIAgents) != len(built) {
		t.Fatalf("catalogue has %d CLIs, spawnllm builds %d", len(config.CLIAgents), len(built))
	}
	for _, c := range config.CLIAgents {
		want, ok := built[c.Protocol]
		if !ok {
			t.Errorf("%s: spawnllm builds no invocation for it", c.Protocol)
			continue
		}
		if !slices.Equal(c.BaseArgs, want) {
			t.Errorf("%s base args = %v, spawnllm runs %v", c.Protocol, c.BaseArgs, want)
		}
		for _, a := range c.TrailingArgs {
			if a != spawnllm.StdinArg {
				t.Errorf("%s trailing arg %q is not spawnllm's stdin marker %q", c.Protocol, a, spawnllm.StdinArg)
			}
		}
	}
}
