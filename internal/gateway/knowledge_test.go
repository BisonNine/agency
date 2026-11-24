// Agency
// License: MIT

package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/kbhost"
)

func hasKBGet(al *agent.AgentLoop) bool {
	inst, ok := al.GetRegistry().GetConfigured("main")
	if !ok {
		return false
	}
	_, ok = inst.Tools.Get("kb_get")
	return ok
}

// The gateway opens one knowledge-base store at start, keeps it across
// reloads (which rebuild the agents' tools from the new config, switch on or
// off) and closes it at shutdown.
func TestGatewayRun_KnowledgeStoreLifetime(t *testing.T) {
	b := bootGatewayWith(t, "", func(s string) string {
		return strings.Replace(s, `"list": [{"id": "main", "default": true}]`,
			`"list": [{"id": "main", "default": true, "knowledge": {"enabled": true}}]`, 1)
	})
	store := b.services.Knowledge
	if store == nil {
		t.Fatal("no knowledge-base store")
	}
	if !hasKBGet(b.al) {
		t.Fatal("main has no kb_get with the switch on")
	}

	reload := func(on bool) {
		t.Helper()
		next, err := b.al.GetConfig().Clone()
		if err != nil {
			t.Fatal(err)
		}
		next.Agents.List[0].Knowledge = &config.KnowledgeConfig{Enabled: on}
		b.run.configReloads <- next
		waitFor(t, "the reloaded switch", func() bool { return hasKBGet(b.al) == on })
		if b.services.Knowledge != store {
			t.Fatal("a reload replaced the store")
		}
	}
	reload(false)
	reload(true)

	main, _ := b.al.GetRegistry().GetConfigured("main")
	resolver := kbhost.New(b.al.GetConfig(), main.Config, main.Workspace)
	ctx := context.Background()
	if _, err := store.Put(ctx, resolver, "notes", "a", kb.Frontmatter{"type": "Note"}, "x\n"); err != nil {
		t.Fatalf("Put while running: %v", err)
	}
	if _, err := store.Search(ctx, resolver, "notes", "x", 1); err != nil {
		t.Fatalf("Search while running: %v", err)
	}

	b.fatal.fatalService("http", errors.New("listener closed"))
	if err := b.stop(t); exitCode(err) != exitCodeServiceDied {
		t.Fatalf("loop = %v", err)
	}
	if _, err := store.Search(ctx, resolver, "notes", "x", 1); err == nil {
		t.Error("the store still serves after shutdown")
	}
	closeKnowledge(nil) // nothing to close
}
