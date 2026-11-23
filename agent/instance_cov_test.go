// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkagent "go.bison9.com/agent"
	"go.bison9.com/agent/ctxengine"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// instCovBlocker is a regular file in a temp dir: a path below it cannot be
// created.
func instCovBlocker(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A fresh agent whose workspace cannot be created is not built.
func TestInstanceCov_FreshWorkspaceFails(t *testing.T) {
	cfg := newTestConfig(t)
	ws := filepath.Join(instCovBlocker(t), "ws")
	spec := agentreg.Spec{
		ID: "fresh-1", Origin: agentreg.OriginTemp, Fresh: true, Workspace: ws, StateDir: ws,
		Config: &config.AgentConfig{ID: "fresh-1", Models: []string{"test-model"}},
	}
	inst, err := newAgentInstance(spec, &cfg.Agents.Defaults, cfg)
	if inst != nil || err == nil || !strings.Contains(err.Error(), "create workspace "+ws) {
		t.Fatalf("newAgentInstance = %v, %v; want the workspace error", inst, err)
	}
}

// An empty reference, or no configuration, resolves to no model.
func TestInstanceCov_ModelLookupEmpty(t *testing.T) {
	if _, _, _, ok := modelListLookup(newTestConfig(t))("  "); ok {
		t.Fatal("an empty reference resolved")
	}
	if _, _, _, ok := modelListLookup(nil)("test-model"); ok {
		t.Fatal("a reference resolved without a configuration")
	}
}

// instCovAgent is an SDK agent whose Close fails.
type instCovAgent struct {
	sdkagent.Agent
	closed chan struct{}
}

func (a *instCovAgent) Close(context.Context) error {
	close(a.closed)
	return errors.New("still busy")
}

// Closing an instance closes its SDK agent in the background; a failure is
// logged.
func TestInstanceCov_CloseAgentFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	a := &instCovAgent{closed: make(chan struct{})}
	inst := &AgentInstance{ID: "alice", turnAgent: a}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	recv(t, a.closed, "the SDK agent to be closed")
	waitFor(t, "the close failure to be logged", func() bool {
		return strings.Contains(logs.String(), "Agent not closed")
	})
}

// A sessions directory that cannot be read refuses the store.
func TestInstanceCov_SessionsDirUnreadable(t *testing.T) {
	dir := instCovBlocker(t)
	_, err := initSessionStore(dir, config.Modes{Dir: 0o700, File: 0o600})
	if err == nil || !strings.Contains(err.Error(), "read sessions directory "+dir) {
		t.Fatalf("initSessionStore = %v, want the read error", err)
	}
}

// Every field of an eviction block overrides the policy.
func TestInstanceCov_ApplyEvictionConfig(t *testing.T) {
	on, protect, evict, budget, args := true, 1, 2, 3, 4
	off := false
	p := ctxengine.DefaultEvictionPolicy()
	applyEvictionConfig(&p, &config.ContextEvictionConfig{
		Enabled: &off, ProtectTurns: &protect, EvictTurns: &evict, BudgetBytes: &budget, NotifyUser: &on, ArgBytes: &args,
	})
	if p.Enabled || p.ProtectTurns != 1 || p.EvictTurns != 2 || p.BudgetBytes != 3 || !p.NotifyUser || p.ArgBytes != 4 {
		t.Fatalf("policy = %+v", p)
	}
}
