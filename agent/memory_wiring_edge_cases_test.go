// ClawEh
// License: MIT

package agent

import (
	"testing"

	"go.bison9.com/agency/config"
)

// Without a configuration no memory is wired.
func TestMemoryWiring_NoConfig(t *testing.T) {
	if mem := (&AgentLoop{}).wireCognitiveMemory(&AgentInstance{ID: "alice", Config: &config.AgentConfig{ID: "alice"}}, "s"); mem != nil {
		t.Fatal("memory was wired without a configuration")
	}
}
