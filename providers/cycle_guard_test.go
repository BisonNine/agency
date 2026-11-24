// Agency
// License: MIT

package providers_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSDKImportsNoAgency enforces the architectural invariant that the agent
// SDK (go.bison9.com/agent: toolspec, toolkit, spawnllm, ctxengine, cogmem,
// accounting, fsmode, modelspec) never imports a host: no package of it may depend on
// Agency. If this fails, an Agency import crept into the SDK and the module
// dependency graph is at risk of a cycle.
func TestSDKImportsNoAgency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.bison9.com/agent/...").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v\n%s", err, out)
	}
	sawSpawnllm, sawToolkit := false, false
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go.bison9.com/agency") {
			t.Fatalf("the agent SDK must not import a host, but depends on %q", line)
		}
		switch line {
		case "go.bison9.com/agent/spawnllm":
			sawSpawnllm = true
		case "go.bison9.com/agent/toolkit":
			sawToolkit = true
		}
	}
	if !sawSpawnllm || !sawToolkit {
		t.Fatalf("go list did not list the SDK's packages; output:\n%s", out)
	}
}
