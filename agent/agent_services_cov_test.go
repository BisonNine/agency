// ClawEh
// License: MIT

package agent

import (
	"errors"
	"testing"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/logger"
)

// Without a registry, or for an unknown caller, there is no caller: no
// models and no fresh agent.
func TestAgentServicesCov_NoCaller(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	bare := newAgentServices(&AgentLoop{}, "alice")
	if _, err := bare.caller(); err == nil || err.Error() != "agent registry is not available" {
		t.Fatalf("caller = %v, want no registry", err)
	}
	if got := bare.AllowedModels(); got != nil {
		t.Fatalf("AllowedModels = %v, want none", got)
	}

	al, _ := hooksCovLoop(t)
	unknown := newAgentServices(al, "nobody")
	if _, err := unknown.caller(); !errors.Is(err, agentreg.ErrNotFound) {
		t.Fatalf("caller = %v, want not found", err)
	}
	if _, err := unknown.CreateFresh("test-model"); !errors.Is(err, agentreg.ErrNotFound) {
		t.Fatalf("CreateFresh = %v, want not found", err)
	}
}
