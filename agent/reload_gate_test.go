// ClawEh
// License: MIT

package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// candidateWires are the names and wire ids of a turn's candidates.
func candidateWires(tm *turnModel) (names, wires []string) {
	for _, c := range tm.candidates {
		names = append(names, c.Name)
		wires = append(wires, c.Model)
	}
	return names, wires
}

// waitFor polls cond until it holds, failing after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// gapConfig is the configuration an adopting reload moves to: a model
// setting changed, main on b, and a new agent bob on c.
func gapConfig(base string) *config.Config {
	cfg := abcConfig(base, "b")
	cfg.Models[0].RequestTimeout = 33
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"c"}})
	return cfg
}

// Between an adopting reload's commit and the adopt, a turn for an agent the
// reload added and a turn for an existing agent wait (listsGate), then get
// exactly their new configuration's lists, with wire ids; a clone created in
// the gap is returned to its creator with its new lists stated.
func TestReload_TurnsWaitForListsAfterCommit(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	next := gapConfig(base)

	// Holding assignMu stops the reload after its commit, before the adopt.
	al.assignMu.Lock()
	reloaded := make(chan error, 1)
	go func() { reloaded <- al.ReloadConfig(context.Background(), next) }()
	waitFor(t, "the commit", func() bool { return al.GetConfig() == next })

	r := al.GetRegistry()
	bob, ok := r.Get("bob")
	if !ok {
		t.Fatal("bob is not registered after the commit")
	}
	main := r.Default()
	turns := map[string]chan *turnModel{"bob": make(chan *turnModel, 1), "main": make(chan *turnModel, 1)}
	for id, inst := range map[string]*AgentInstance{"bob": bob, "main": main} {
		go func() { turns[id] <- al.selectTurnModel(inst, "s") }()
	}
	type created struct {
		id   string
		chat []string
		err  error
	}
	clone := make(chan created, 1)
	go func() {
		id, err := r.CreateClone("main")
		c := created{id: id, err: err}
		if err == nil {
			c.chat = entryNames(al.catalog.Set(id).List(models.Chat))
		}
		clone <- c
	}()
	// The clone waits for the adopt in its insert hook, once inserted.
	waitFor(t, "the clone's insertion", func() bool { return len(r.All()) == 3 })
	select {
	case tm := <-turns["bob"]:
		t.Fatalf("bob's turn chose %+v before its lists were stated", tm)
	case tm := <-turns["main"]:
		t.Fatalf("main's turn chose %+v before its lists were stated", tm)
	case c := <-clone:
		t.Fatalf("the clone was created before its lists were stated: %+v", c)
	case <-time.After(20 * time.Millisecond):
	}
	al.assignMu.Unlock()
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{"bob": "c", "main": "b"} {
		tm := <-turns[id]
		names, wires := candidateWires(tm)
		if !slices.Equal(names, []string{want}) || !slices.Equal(wires, []string{want + "-wire"}) || tm.model != want {
			t.Errorf("%s's turn = %v / %v on %q, want [%s] / [%s-wire]", id, names, wires, tm.model, want, want)
		}
	}
	c := <-clone
	if c.err != nil {
		t.Fatal(c.err)
	}
	if !slices.Equal(c.chat, []string{"b"}) {
		t.Errorf("the clone's chat list when its id was returned = %v, want [b]", c.chat)
	}
}

// Two reloads at once end with the catalogue and every agent's lists of the
// configuration the registry ended on.
func TestReload_ConcurrentReloadsAgree(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	cfgs := [2]*config.Config{abcConfig(base, "a", "b"), gapConfig(base)}
	al := mustNewAgentLoop(t, abcConfig(base, "c"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	for range 8 {
		var wg sync.WaitGroup
		for _, cfg := range cfgs {
			wg.Go(func() {
				if err := al.ReloadConfig(context.Background(), cfg); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		final := al.GetConfig()
		if al.currentDigest() != catalogDigest(final) {
			t.Fatal("the catalogue is not the one of the configuration in force")
		}
		for _, inst := range registryInstances(al.GetRegistry()) {
			if inst.builtWith != final {
				t.Fatalf("%s was built with another configuration", inst.ID)
			}
		}
		checkAllAssigned(t, al)
		if info, _ := al.catalog.Get("a"); info.Model.RequestTimeout != final.Models[0].RequestTimeout {
			t.Fatalf("model a in the catalogue = %+v, want the final configuration's", info.Model)
		}
	}
}

// listsComplete fails unless the set of id has a chat list of wire models.
func listsComplete(t *testing.T, al *AgentLoop, inst *AgentInstance, what string) {
	t.Helper()
	tm := al.selectTurnModel(inst, "s")
	_, wires := candidateWires(tm)
	if len(wires) == 0 {
		t.Errorf("%s: %s has no chat model", what, inst.ID)
		return
	}
	for _, w := range wires {
		if !strings.HasSuffix(w, "-wire") {
			t.Errorf("%s: %s sends %q, not a wire id", what, inst.ID, w)
		}
	}
}

// Creations and turns racing reloads that add an agent and adopt find the
// lists complete when the creator receives the id and at each turn start.
// A turn starts only on an agent whose id is known: a configured one or a
// clone its creator has received.
func TestReload_RaceListsCompleteAtEveryTurn(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	cfgs := [2]func() *config.Config{
		func() *config.Config { return abcConfig(base, "a", "b") },
		func() *config.Config { return gapConfig(base) },
	}
	al := mustNewAgentLoop(t, cfgs[0](), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	var stop atomic.Bool
	var known sync.Map // ids a turn may start on
	known.Store("main", true)
	known.Store("bob", true)
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			id, err := al.GetRegistry().CreateClone("main")
			if err != nil {
				t.Errorf("CreateClone: %v", err)
				return
			}
			if inst, ok := al.GetRegistry().Get(id); ok {
				listsComplete(t, al, inst, "at creation")
			}
			known.Store(id, true)
		}
	})
	wg.Go(func() {
		for !stop.Load() {
			for _, inst := range registryInstances(al.GetRegistry()) {
				if _, ok := known.Load(inst.ID); !ok {
					continue
				}
				// As a turn does: the agent stays until the turn ends.
				end, ok := al.GetRegistry().BeginTurn(inst.ID, inst)
				if !ok {
					continue
				}
				listsComplete(t, al, inst, "at a turn")
				end()
			}
		}
	})
	for i := range 12 {
		if err := al.ReloadConfig(context.Background(), cfgs[(i+1)%2]()); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	checkAllAssigned(t, al)
}

// closedWriteStore records model choices saved through it after Close.
type closedWriteStore struct {
	session.SessionStore
	state  compactionStateStore
	closed atomic.Bool
	late   atomic.Int32
}

func (s *closedWriteStore) Close() error {
	s.closed.Store(true)
	return s.SessionStore.Close()
}

func (s *closedWriteStore) GetCompactionState(key string) (memory.CompactionState, error) {
	return s.state.GetCompactionState(key)
}

func (s *closedWriteStore) SetCompactionState(key string, st memory.CompactionState) error {
	if s.closed.Load() {
		s.late.Add(1)
	}
	return s.state.SetCompactionState(key, st)
}

// A reload that moves a session's chosen model re-saves the choice through
// the new instance, never the replaced (closed) one.
func TestReload_ChoiceSavedThroughNewInstance(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a", "b", "c"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	main := al.GetRegistry().Default()
	selectModel(t, al, main, "s", "c")
	state, ok := main.Sessions.(compactionStateStore)
	if !ok {
		t.Fatal("no compaction state store")
	}
	old := &closedWriteStore{SessionStore: main.Sessions, state: state}
	main.Sessions = old
	al.selectionAgents.Store(main.ID, main)

	if err := al.ReloadConfig(context.Background(), abcConfig(base, "b", "c", "a")); err != nil {
		t.Fatal(err)
	}
	if n := old.late.Load(); n != 0 {
		t.Fatalf("%d choice(s) saved through the replaced instance after it was closed", n)
	}
	now := al.GetRegistry().Default()
	if now == main {
		t.Fatal("the reload did not replace main")
	}
	if got := storedIndex(t, now, "s"); got != 1 {
		t.Fatalf("stored index = %d, want 1 (c in [b c a])", got)
	}
}
