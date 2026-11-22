// ClawEh
// License: MIT

package agent

import (
	"slices"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/routing"
)

// A realistic sessions folder lists the sessions it always listed after the
// agent has run: the main session, a per-sender session an earlier release
// wrote (old-format key), a temporary agent's main session in its own
// folder; an archive an earlier release wrote with no session state (only
// archived messages) is not a session, before or after the agent's own
// session is opened and written. Retention judges the same set, and the
// store's pending list (what restart recovery reads) holds only a session
// marked pending.
func TestSessionListing_RealisticDataDir(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}}
	msgBus := bus.NewMessageBus()
	al := mustNewAgentLoop(t, cfg, msgBus, &fixedProvider{resp: &providers.LLMResponse{Content: "hi"}}, nil, OwnsDataDir())
	alice, _ := al.GetRegistry().Get("alice")
	dir := alice.SessionsDir()

	const oldKey = "agent:alice:telegram:direct:555"
	const archiveOnlyKey = "agent:alice:telegram:direct:777"
	mainKey := routing.BuildAgentMainSessionKey("alice")

	// An earlier release's per-sender session, written by the store.
	addFullMessage(t, alice.Sessions, oldKey, providers.Message{Role: "user", Content: "old"})
	// An earlier release's archive with no session state.
	a, err := memory.Open(memory.ArchivePath(dir, archiveOnlyKey))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Append(1, spawnllm.Message{Role: "user", Content: "archived"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	closeT(t, a)

	list := func(d string) []string {
		t.Helper()
		keys, listErr := memory.ListSessions(d, logger.SDK())
		if listErr != nil {
			t.Fatal(listErr)
		}
		return keys
	}
	if got := list(dir); !slices.Equal(got, []string{oldKey}) {
		t.Fatalf("listed before the agent ran = %q, want [%s]", got, oldKey)
	}

	// The agent runs a turn in its main session (store, archive and the
	// archive's prune at open all write it).
	dispatch(al, inbound("c1", "id1", "hello"))
	if out := nextOutbound(t, msgBus); out.Content != "hi" {
		t.Fatalf("reply = %q", out.Content)
	}
	want := []string{mainKey, oldKey}
	if got := list(dir); !slices.Equal(got, want) {
		t.Fatalf("listed after a turn = %q, want %q", got, want)
	}

	// A temporary agent's session is listed in its own folder.
	id, err := al.GetRegistry().CreateClone("alice")
	if err != nil {
		t.Fatal(err)
	}
	clone, _ := al.GetRegistry().Get(id)
	cloneKey := routing.BuildAgentMainSessionKey(id)
	addFullMessage(t, clone.Sessions, cloneKey, providers.Message{Role: "user", Content: "temp"})
	if got := list(clone.SessionsDir()); !slices.Equal(got, []string{cloneKey}) {
		t.Fatalf("temporary agent's sessions = %q, want [%s]", got, cloneKey)
	}
	if got := list(dir); !slices.Equal(got, want) {
		t.Fatalf("alice's sessions after the clone = %q, want %q", got, want)
	}

	// Nothing is pending after the turn; a marked session is.
	pending, err := alice.Sessions.ListPendingSessions()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %q, %v; want none", pending, err)
	}
	bobDir := t.TempDir()
	seedRetentionArchive(t, bobDir, "agent:bob:main", time.Now(), true)
	bobStore, err := session.NewSQLiteStore(bobDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeT(t, bobStore)
	if pending, err := bobStore.ListPendingSessions(); err != nil || !slices.Equal(pending, []string{"agent:bob:main"}) {
		t.Fatalf("pending = %q, %v; want [agent:bob:main]", pending, err)
	}

	// Retention judges the listed sessions: far in the future, the old
	// per-sender one goes (the main session is exempt); the archive with no
	// state is left alone.
	rep := pruneSessions(dir, alice.Sessions, time.Now().Add(400*24*time.Hour), 30, nil)
	if len(rep.errors) != 0 || !slices.Equal(rep.deleted, []string{oldKey}) {
		t.Fatalf("retention deleted %q (errors %v), want [%s]", rep.deleted, rep.errors, oldKey)
	}
	if !archiveExists(t, dir, archiveOnlyKey) || !archiveExists(t, dir, mainKey) {
		t.Fatal("retention removed a file it must leave")
	}
}
