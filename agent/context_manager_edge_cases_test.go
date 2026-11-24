// Agency
// License: MIT

package agent

import (
	"context"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
)

// A caller with a workspace runs its chain isolated there and still answers.
func TestContextManager_IsolatedChain(t *testing.T) {
	p := &scriptedProvider{content: "summary"}
	c := compressCaller(compressCatalog(map[string]spawnllm.LLMProvider{"model-a": p}), "model-a")
	c.workspace = t.TempDir()
	reply, err := c.Complete(context.Background(), hook.CompleteRequest{System: "s", User: "u"})
	if err != nil || reply.Content != "summary" || reply.Model != "model-a" {
		t.Fatalf("Complete = %+v, %v; want model-a's reply", reply, err)
	}
}

// A provider is named only for a model the configuration has.
func TestContextManager_CompressProviderName(t *testing.T) {
	if got := compressProviderName(nil, "test-model"); got != "" {
		t.Fatalf("without a configuration = %q, want none", got)
	}
	cfg := newTestConfig(t)
	if got := compressProviderName(cfg, "no-such-model"); got != "" {
		t.Fatalf("unknown model = %q, want none", got)
	}
	if got := compressProviderName(cfg, "test-model"); got != testProviderName {
		t.Fatalf("test-model = %q, want %q", got, testProviderName)
	}
}
