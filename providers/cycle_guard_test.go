// ClawEh
// License: MIT

package providers_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSDKImportsNoClawEh enforces the architectural invariant that the agent
// SDK (go.bison9.com/agent: toolspec, spawnllm, ctxengine, cogmem, accounting,
// fsmode) never imports a host: no package of it may depend on ClawEh. If this
// fails, a ClawEh import crept into the SDK and the module dependency graph is
// at risk of a cycle.
func TestSDKImportsNoClawEh(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.bison9.com/agent/...").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v\n%s", err, out)
	}
	sawSpawnllm := false
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go.bison9.com/agency") {
			t.Fatalf("the agent SDK must not import a host, but depends on %q", line)
		}
		if line == "go.bison9.com/agent/spawnllm" {
			sawSpawnllm = true
		}
	}
	if !sawSpawnllm {
		t.Fatalf("go list did not list the SDK's packages; output:\n%s", out)
	}
}
