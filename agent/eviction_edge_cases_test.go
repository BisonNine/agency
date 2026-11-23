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
	"time"

	"go.bison9.com/agent/ctxengine/memory"

	"go.bison9.com/agency/logger"
)

// evictEdgeCM is a context manager whose Close reports on closed and returns
// err.
type evictEdgeCM struct {
	trackingContextManager
	err    error
	closed chan struct{}
}

func newEvictEdgeCM(err error) *evictEdgeCM {
	return &evictEdgeCM{err: err, closed: make(chan struct{})}
}

func (m *evictEdgeCM) Close(context.Context) error {
	m.trackingContextManager.closed.Store(true)
	close(m.closed)
	return m.err
}

// evictEdgeSTI records the session keys it revokes.
type evictEdgeSTI struct {
	recordingSTI
	revoked []string
}

func (s *evictEdgeSTI) Revoke(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, key)
}

func (s *evictEdgeSTI) revokedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.revoked...)
}

// The idle-eviction goroutine evicts on its ticker and stops when told to.
func TestEviction_GoroutineEvictsAndStops(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := &AgentLoop{evictStop: make(chan struct{}), evictTTL: 0, evictInterval: time.Millisecond}
	cm := newEvictEdgeCM(nil)
	makeEntry(al, "a:main", cm, time.Now().Add(-time.Hour), 0)

	stopped := make(chan struct{})
	go func() {
		al.evictContextManagers()
		close(stopped)
	}()
	recv(t, cm.closed, "the idle entry to be evicted")
	close(al.evictStop)
	recv(t, stopped, "the eviction goroutine to stop")
	if _, ok := al.contextManagers.Load("a:main"); ok {
		t.Fatal("the idle entry is still cached")
	}
}

// Without a cached entry nothing is reissued.
func TestEviction_ReissueWithoutEntry(t *testing.T) {
	sti := &countingSTI{}
	al := &AgentLoop{sessionTokenIssuer: sti}
	al.reissueSessionToken(&AgentInstance{ID: "alice"}, "main")
	if n := sti.count(); n != 0 {
		t.Fatalf("issued %d tokens, want none", n)
	}
}

// Entries that are not cache entries are skipped by the idle pass and
// dropped by a reload.
func TestEviction_ForeignValues(t *testing.T) {
	al := &AgentLoop{}
	al.contextManagers.Store("x:main", "not an entry")
	al.runEvictionPass(0)
	if _, ok := al.contextManagers.Load("x:main"); !ok {
		t.Fatal("the idle pass removed a value it does not own")
	}
	al.invalidateContextManagers(context.Background())
	if _, ok := al.contextManagers.Load("x:main"); ok {
		t.Fatal("a reload kept a value that is not an entry")
	}
}

// An entry replaced in the cache, or taken by a caller after it was marked,
// is not evicted.
func TestEviction_TryEvictSkips(t *testing.T) {
	al := &AgentLoop{}
	old := &cmEntry{cm: &trackingContextManager{}}
	current := makeEntry(al, "a:main", &trackingContextManager{}, time.Now(), 0)
	if al.tryEvictEntry(context.Background(), "a:main", old, evictReasonIdle) {
		t.Fatal("evicted an entry no longer cached")
	}
	current.refcount.Store(1)
	if al.tryEvictEntry(context.Background(), "a:main", current, evictReasonIdle) {
		t.Fatal("evicted an entry in use")
	}
	if !current.stale.Load() {
		t.Fatal("the entry in use was not marked stale")
	}
	if v, _ := al.contextManagers.Load("a:main"); v != current {
		t.Fatal("the entry in use left the cache")
	}
}

// A manager that fails to close on eviction is logged and still evicted.
func TestEviction_EvictCloseFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al := &AgentLoop{}
	cm := newEvictEdgeCM(errors.New("disk gone"))
	makeEntry(al, "a:main", cm, time.Now(), 0)
	if !al.dropContextManager(context.Background(), &AgentInstance{ID: "a"}, "main", evictReasonReleased) {
		t.Fatal("the idle entry was not dropped")
	}
	if !strings.Contains(logs.String(), "eviction: context manager close failed") {
		t.Fatalf("the close failure was not logged: %s", logs.String())
	}
}

// The shutdown drain revokes each session's token, logs a manager that fails
// to close, and skips values that are not entries.
func TestEviction_DrainRevokesAndLogs(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	sti := &evictEdgeSTI{}
	al := &AgentLoop{sessionTokenIssuer: sti}
	cm := newEvictEdgeCM(errors.New("disk gone"))
	entry := makeEntry(al, "a:main", cm, time.Now(), 0)
	entry.sessionKey = "main"
	al.contextManagers.Store("x:main", "not an entry")

	al.drainContextManagers(context.Background())

	recv(t, cm.closed, "the manager to be closed")
	if !strings.Contains(logs.String(), "shutdown drain: context manager close failed") {
		t.Fatalf("the close failure was not logged: %s", logs.String())
	}
	if got := sti.revokedKeys(); len(got) != 1 || got[0] != "main" {
		t.Fatalf("revoked = %v, want [main]", got)
	}
	if _, ok := al.contextManagers.Load("x:main"); !ok {
		t.Fatal("the drain removed a value it does not own")
	}
}

// A discard leaves a value that is not an entry alone and logs an archive
// it cannot delete.
func TestEviction_DiscardConversation(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()

	al.contextManagers.Store(agent.ID+":foreign", "not an entry")
	al.discardConversation(context.Background(), agent, "foreign")
	if _, ok := al.contextManagers.Load(agent.ID + ":foreign"); !ok {
		t.Fatal("the discard removed a value it does not own")
	}

	// A directory where the archive should be cannot be removed.
	archive := memory.ArchivePath(agent.SessionsDir(), "stuck")
	if err := os.MkdirAll(filepath.Join(archive, "inside"), 0o750); err != nil {
		t.Fatal(err)
	}
	al.discardConversation(context.Background(), agent, "stuck")
	if !strings.Contains(logs.String(), "Failed to delete a single-shot conversation") {
		t.Fatalf("the failed delete was not logged: %s", logs.String())
	}
}
