// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// abcConfig is a config whose default agent, main, runs on models a, b and
// c in the order given, in baseDir.
func abcConfig(baseDir string, order ...string) *config.Config {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			BaseDir:  baseDir,
			Defaults: config.AgentDefaults{Models: order, MaxTokens: 4096, MaxToolIterations: 10},
			List:     []config.AgentConfig{{ID: "main", Name: "Main", Default: true}},
		},
		Providers: []config.Provider{{Name: "p", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}},
	}
	for _, n := range []string{"a", "b", "c"} {
		cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: n, Model: n + "-wire", Provider: "p", Enabled: true})
	}
	return cfg
}

// restartOn closes al's agents and builds a new loop on cfg, as a restart
// does, returning it and its default agent.
func restartOn(t *testing.T, al *AgentLoop, cfg *config.Config) (*AgentLoop, *AgentInstance) {
	t.Helper()
	r := al.GetRegistry()
	r.Close()
	for _, id := range r.All() {
		if inst, ok := r.Get(id); ok {
			if err := inst.Close(); err != nil {
				t.Fatalf("close %s: %v", id, err)
			}
		}
	}
	next := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	return next, next.GetRegistry().Default()
}

// selectModel makes model the session's /model choice.
func selectModel(t *testing.T, al *AgentLoop, agent *AgentInstance, session, model string) {
	t.Helper()
	idx := slices.Index(al.chatModelNames(agent), model)
	if err := al.setActiveModelIndex(agent, session, idx); err != nil {
		t.Fatalf("select %s: %v", model, err)
	}
}

// A /model choice is kept by model, not by position: a reload that reorders
// the list keeps the session on its model, and so does the restart after it;
// a reload that removes the chosen model puts the session back on the first
// model, and the restart after it agrees.
func TestModelChoice_ReloadThenRestart(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	const session = "agent:main:main"
	for _, tc := range []struct {
		name  string
		after []string
		want  string
	}{
		{"reordered", []string{"c", "a", "b"}, "c"},
		{"removed", []string{"a", "b"}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			al := mustNewAgentLoop(t, abcConfig(base, "a", "b", "c"), bus.NewMessageBus(), &mockProvider{}, nil)
			selectModel(t, al, al.GetRegistry().Default(), session, "c")

			if err := al.ReloadConfig(context.Background(), abcConfig(base, tc.after...)); err != nil {
				t.Fatal(err)
			}
			if got := al.activeModelName(al.GetRegistry().Default(), session); got != tc.want {
				t.Fatalf("after the reload the session runs on %q, want %q", got, tc.want)
			}
			next, agent := restartOn(t, al, abcConfig(base, tc.after...))
			if got := next.activeModelName(agent, session); got != tc.want {
				t.Fatalf("after the restart the session runs on %q, want %q", got, tc.want)
			}
		})
	}
}

// A reload that fails after the agents were built leaves every agent on its
// old lists.
func TestReloadFailure_KeepsOldLists(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	cfg := abcConfig(base, "a", "b", "c")
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob"})
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	bob, _ := al.GetRegistry().Get("bob")

	// Bob's rebuild fails: his sessions directory holds an unmigrated
	// session. Main is rebuilt first, with a new list.
	if err := os.WriteFile(filepath.Join(bob.SessionsDir(), "x.meta.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := abcConfig(base, "c", "b")
	next.Agents.List = append(next.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob"})
	if err := al.ReloadConfig(context.Background(), next); err == nil {
		t.Fatal("the reload succeeded")
	}
	if got := al.chatModelNames(al.GetRegistry().Default()); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("main's list after the failed reload = %v, want the old one", got)
	}
}

// Turns that begin while reloads replace an agent's list see the old list
// or the new one, never an empty or partial one, and the session's choice
// stays valid (run with -race).
func TestReload_TurnsNeverSeePartialLists(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a", "b", "c"), bus.NewMessageBus(), &mockProvider{}, nil)
	const session = "s"
	selectModel(t, al, al.GetRegistry().Default(), session, "b")

	var stop atomic.Bool
	var bad atomic.Value
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				tm := al.selectTurnModel(al.GetRegistry().Default(), session)
				names := make([]string, 0, len(tm.candidates))
				for _, c := range tm.candidates {
					names = append(names, c.Name)
				}
				slices.Sort(names)
				if !slices.Equal(names, []string{"a", "b", "c"}) || tm.model != "b" {
					bad.Store(names)
					return
				}
			}
		})
	}
	for i := range 20 {
		order := []string{"a", "b", "c"}
		if i%2 == 1 {
			order = []string{"c", "b", "a"}
		}
		if err := al.ReloadConfig(context.Background(), abcConfig(base, order...)); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatalf("a turn saw the list %v or lost its choice", v)
	}
}

// failingStateStore is a session store whose compaction state cannot be
// read.
type failingStateStore struct{ compactionStateStore }

func (failingStateStore) GetCompactionState(string) (memory.CompactionState, error) {
	return memory.CompactionState{}, errors.New("disk on fire")
}

// LoadSelection: an agent the registry does not hold yet is "not read yet"
// (an error, read again later); a session whose state cannot be read has no
// saved choice, as before.
func TestSessionSelections_Load(t *testing.T) {
	al, agent := fourModelLoop(t)
	st := sessionSelections{al}
	if _, err := st.LoadSelection("nobody", models.Chat, "s"); err == nil {
		t.Error("an agent not in the registry loaded a selection")
	}
	selectModel(t, al, agent, "s", "a2")
	if name, err := st.LoadSelection(agent.ID, models.Chat, "s"); err != nil || name != "a2" {
		t.Fatalf("load = %q, %v; want a2", name, err)
	}
	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		t.Fatal("no compaction state store")
	}
	agent.Sessions = failingSessions{SessionStore: agent.Sessions, state: failingStateStore{store}}
	if name, err := st.LoadSelection(agent.ID, models.Chat, "s"); err != nil || name != "" {
		t.Fatalf("unreadable state: load = %q, %v; want no choice", name, err)
	}
}

// One health for every model call: a model parked by a chat failure is
// skipped by a task-model call, one parked by a task-model failure is
// skipped by a turn, and /cooldowns lists it once and clears it.
func TestSharedHealth_ChatAndTaskModels(t *testing.T) {
	e := newTaskHostEnv(t)
	e.vendor.fail["chat-a-wire"] = 503
	// A turn of alice (on chat-a) fails over: chat-a is parked.
	alice, _ := e.al.GetRegistry().Get("alice")
	tm := e.al.selectTurnModel(alice, "s")
	if _, err := tm.chain.Chat(context.Background(), alice.Identity(), models.Request{}); err == nil {
		t.Fatal("the turn's only model answered")
	}
	if !parkedIn(e.al.catalog, "p", "chat-a-wire") {
		t.Fatal("a 503 in a turn did not park chat-a")
	}
	before := len(e.vendor.calls())
	res, err := e.run(t, "alice", toolsagentsText("hi"))
	if err != nil || res.Model != "chat-b" || len(e.vendor.calls()) != before+1 {
		t.Fatalf("task call after the chat failure: %+v, %v (calls %d)", res, err, len(e.vendor.calls())-before)
	}
	rt := &commands.Runtime{}
	e.al.addCooldownHooks(rt)
	if got := rt.ListCooldowns(); len(got) != 1 || got[0].Model != "chat-a-wire" {
		t.Fatalf("/cooldowns = %+v, want chat-a once", got)
	}
	if !rt.ClearCooldown("p", "chat-a-wire") || len(rt.ListCooldowns()) != 0 {
		t.Fatal("/cooldowns clear did not clear chat-a")
	}

	// A task-model failure the cooldown rule accepts parks chat-a for turns.
	if _, err := e.run(t, "alice", toolsagentsText("hi")); err != nil {
		t.Fatal(err)
	}
	if !parkedIn(e.al.catalog, "p", "chat-a-wire") {
		t.Fatal("a 503 in a task call did not park chat-a")
	}
	before = len(e.vendor.calls())
	if _, err := e.al.selectTurnModel(alice, "s").chain.Chat(context.Background(), alice.Identity(), models.Request{}); err == nil {
		t.Fatal("the parked model answered")
	}
	if len(e.vendor.calls()) != before {
		t.Fatal("a turn called the model a task call parked")
	}
}

// failingSessions is a session store whose compaction state comes from
// state.
type failingSessions struct {
	session.SessionStore
	state compactionStateStore
}

func (f failingSessions) GetCompactionState(key string) (memory.CompactionState, error) {
	return f.state.GetCompactionState(key)
}

func (f failingSessions) SetCompactionState(key string, st memory.CompactionState) error {
	return f.state.SetCompactionState(key, st)
}

// toolsagentsText is a text task request for prompt.
func toolsagentsText(prompt string) toolsagents.TaskModelRequest {
	return toolsagents.TaskModelRequest{Kind: "text", Prompt: prompt}
}
