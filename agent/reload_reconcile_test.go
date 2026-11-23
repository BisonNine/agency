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

	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// wantChat is the chat list agent inst should have: its own resolved
// models that the catalogue has.
func wantChat(al *AgentLoop, inst *AgentInstance) []string {
	var out []string
	for _, n := range inst.chatModels {
		if _, ok := al.catalog.Get(n); ok {
			out = append(out, n)
		}
	}
	return out
}

// checkAllAssigned fails unless every agent the registry holds has exactly
// its own chat list and can start a turn.
func checkAllAssigned(t *testing.T, al *AgentLoop) {
	t.Helper()
	for _, inst := range registryInstances(al.GetRegistry()) {
		got := entryNames(al.catalog.Set(inst.ID).List(models.Chat))
		if want := wantChat(al, inst); !slices.Equal(got, want) {
			t.Errorf("%s: chat list %v, want %v", inst.ID, got, want)
		}
		if tm := al.selectTurnModel(inst, "s"); len(tm.candidates) == 0 {
			t.Errorf("%s: a turn has no model", inst.ID)
		}
	}
}

// raceReloads creates temporary agents while reloads alternate between two
// configurations, then checks every agent.
func raceReloads(t *testing.T, cfgs [2]func() *config.Config) {
	t.Helper()
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, cfgs[0](), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if _, err := al.GetRegistry().CreateClone("main"); err != nil {
				t.Errorf("CreateClone: %v", err)
				return
			}
		}
	})
	for i := range 12 {
		if err := reloadForTest(t, al, context.Background(), &mockProvider{}, cfgs[(i+1)%2]()); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	checkAllAssigned(t, al)
}

// (a) Creations racing reloads that adopt a new catalogue end with their own
// lists and can run a turn.
func TestReload_CreationsRaceAdoptingReloads(t *testing.T) {
	base := t.TempDir()
	raceReloads(t, [2]func() *config.Config{
		func() *config.Config { return abcConfig(base, "a", "b") },
		func() *config.Config {
			c := abcConfig(base, "a", "b")
			c.Models[0].RequestTimeout = 42
			return c
		},
	})
}

// (b) The same with reloads that change only the lists.
func TestReload_CreationsRaceListReloads(t *testing.T) {
	base := t.TempDir()
	raceReloads(t, [2]func() *config.Config{
		func() *config.Config { return abcConfig(base, "a", "b") },
		func() *config.Config { return abcConfig(base, "b", "c") },
	})
}

// storedIndex is the /model index the session holds on disk.
func storedIndex(t *testing.T, inst *AgentInstance, session string) int {
	t.Helper()
	store, ok := inst.Sessions.(compactionStateStore)
	if !ok {
		t.Fatal("no compaction state store")
	}
	st, err := store.GetCompactionState(session)
	if err != nil {
		t.Fatal(err)
	}
	return st.ActiveModelIndex
}

// cancelOnBuild is a tool provider whose Build runs hook, if set: it lets a
// test act while a reload builds its agents.
type cancelOnBuild struct{}

var buildHook atomic.Pointer[func()]

func (cancelOnBuild) Namespace() string                       { return "reloadhook" }
func (cancelOnBuild) Description() string                     { return "test" }
func (cancelOnBuild) Category() string                        { return "test" }
func (cancelOnBuild) ConfigKey() string                       { return "" }
func (cancelOnBuild) Available(*config.Config) (bool, string) { return true, "" }
func (cancelOnBuild) Describe() []tools.ToolDescriptor        { return nil }
func (cancelOnBuild) Build(tools.ToolDeps) []toolkit.Tool {
	if h := buildHook.Load(); h != nil {
		(*h)()
	}
	return nil
}

// useBuildHook registers cancelOnBuild for the test only.
func useBuildHook(t *testing.T) {
	t.Helper()
	tools.RegisterProvider(cancelOnBuild{})
	t.Cleanup(func() {
		buildHook.Store(nil)
		tools.UnregisterProvider(cancelOnBuild{}.Namespace())
	})
}

// (c)(d) A reload that fails to build, or whose context ends while it
// builds, changes no agent's list and no stored /model choice.
func TestReload_FailedOrAbandonedChangesNothing(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	useBuildHook(t)
	for _, tc := range []string{"adopting", "lists only", "build error"} {
		t.Run(tc, func(t *testing.T) {
			base := t.TempDir()
			cfg := abcConfig(base, "a", "b", "c")
			cfg.Agents.List[0].Tools = []string{"*"}
			al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
			main := al.GetRegistry().Default()
			selectModel(t, al, main, "s", "c")
			next := abcConfig(base, "c", "b")
			next.Agents.List[0].Tools = []string{"*"}
			if tc == "adopting" {
				next.Models[0].RequestTimeout = 9
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			if tc == "build error" {
				hook := func() { panic("tool build failed") }
				buildHook.Store(&hook)
			} else {
				// The reload is abandoned while it builds: the build stays
				// blocked until ReloadConfig has returned.
				var once sync.Once
				hook := func() { once.Do(func() { cancel(); <-release }) }
				buildHook.Store(&hook)
			}
			err := al.ReloadConfig(ctx, next)
			buildHook.Store(nil)
			close(release)
			if err == nil {
				t.Fatal("the reload succeeded")
			}
			// Wait for the abandoned build to end: a creation waits for it.
			id, cerr := al.GetRegistry().CreateClone("main")
			if cerr != nil {
				t.Fatal(cerr)
			}
			if err := al.GetRegistry().Delete(id); err != nil {
				t.Fatal(err)
			}
			if al.GetConfig() != cfg {
				t.Fatal("the reload committed")
			}
			main = al.GetRegistry().Default()
			if got := al.chatModelNames(main); !slices.Equal(got, []string{"a", "b", "c"}) {
				t.Fatalf("lists after the failed reload = %v", got)
			}
			if got := al.activeModelName(main, "s"); got != "c" || storedIndex(t, main, "s") != 2 {
				t.Fatalf("choice after the failed reload = %q / %d", got, storedIndex(t, main, "s"))
			}
		})
	}
}

// panicOnClose is a session store whose Close panics.
type panicOnClose struct{ session.SessionStore }

func (panicOnClose) Close() error { panic("store close") }

// (f) A reload that fails after it committed (here: closing a replaced
// instance panics) still states every agent's lists.
func TestReload_PanicAfterCommitStillAssigns(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a", "b"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	id, err := al.GetRegistry().CreateClone("main")
	if err != nil {
		t.Fatal(err)
	}
	clone, _ := al.GetRegistry().Get(id)
	clone.Sessions = panicOnClose{clone.Sessions}
	err = reloadForTest(t, al, context.Background(), &mockProvider{}, abcConfig(base, "b", "c"))
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err = %v, want the panic", err)
	}
	checkAllAssigned(t, al)
	if got := al.chatModelNames(al.GetRegistry().Default()); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("main after the committed reload = %v, want [b c]", got)
	}
}

// A waiting human turn does not hold the catalogue: closing it at shutdown
// is prompt.
func TestHumanTurnWaiting_CloseIsPrompt(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _, _ := newHumanLoop(t, 600)
	bob, _ := al.GetRegistry().Get("bob")
	al.asks.open("ask-1", "Alice", al.clk().Now().Add(time.Hour), false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = al.runAgentLoop(context.Background(), bob, processOptions{ //nolint:errcheck // ends when the test stops it
			SessionKey: "agent:bob:main", Channel: "agent_message", ChatID: "ask-1", UserMessage: "q", ReplyRequired: true,
		})
	}()
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := al.CloseModels(ctx); err != nil {
		t.Fatalf("CloseModels: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("closing the catalogue waited for the person")
	}
}
