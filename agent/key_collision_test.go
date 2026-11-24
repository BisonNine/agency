// Agency
// License: MIT

package agent

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// A session file that holds another session's key (two keys naming one
// file) is refused by the store. The host never makes such keys; should one
// happen, the turn ends with an error reply, logged, without a crash or a
// retry loop, and the log does not name the other session's key.
func TestSessionKeyCollision_TurnFails(t *testing.T) {
	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	cfg := newTestConfig(t)
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}}
	msgBus := bus.NewMessageBus()
	p := &replayProvider{errs: map[string]string{}}
	al := mustNewAgentLoop(t, cfg, msgBus, p, nil, OwnsDataDir())
	alice, _ := al.GetRegistry().Get("alice")

	// Another key that names the same file claims it first.
	const otherKey = "agent/alice/main"
	other, err := session.NewSQLiteStore(alice.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	addFullMessage(t, other, otherKey, spawnllm.Message{Role: "user", Content: "private"})
	closeT(t, other)

	dispatch(al, inbound("c1", "id1", "hello"))
	out := nextOutbound(t, msgBus)
	if out.Outcome != bus.OutcomeError {
		t.Fatalf("reply = %q (outcome %q), want an error outcome", out.Content, out.Outcome)
	}
	al.activeRequests.Wait()
	noOutbound(t, msgBus)
	logged := false
	for _, rec := range readLogRecords(t, buf) {
		if rec["level"] == "error" && rec["message"] == "Session store write failed" && rec["session_key"] == "agent:alice:main" &&
			strings.Contains(fmt.Sprint(rec["error"]), "the file holds another session") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("the refusal is not logged as an error:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), otherKey) {
		t.Fatalf("the log names the other session's key:\n%s", buf.String())
	}
	// The other session's file is untouched: still its key, its message only.
	a, err := memory.OpenReadOnly(memory.ArchivePath(alice.SessionsDir(), otherKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeT(t, a)
	if st, stErr := a.State(); stErr != nil || st.Key != otherKey {
		t.Fatalf("other file's key = %q, %v; want %q", st.Key, stErr, otherKey)
	}
	reread, err := session.NewSQLiteStore(alice.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeT(t, reread)
	if h := reread.GetHistory(otherKey); len(h) != 1 || h[0].Content != "private" {
		t.Fatalf("other session's history = %+v; want only its own message", h)
	}
	if got := p.calls(); len(got) > 1 {
		t.Fatalf("model calls = %q, want at most one (no retry loop)", got)
	}
}
