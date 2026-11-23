// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	cogmemstore "go.bison9.com/agent/cogmem/store"
	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/cogmemhost"
	"go.bison9.com/agency/logger"
)

// sessStateCovUnreadable is a session store whose compaction state can be
// neither read nor written; it counts the writes attempted.
type sessStateCovUnreadable struct {
	session.SessionStore
	sets int
}

func (*sessStateCovUnreadable) GetCompactionState(string) (memory.CompactionState, error) {
	return memory.CompactionState{}, errors.New("database is locked")
}

func (s *sessStateCovUnreadable) SetCompactionState(string, memory.CompactionState) error {
	s.sets++
	return errors.New("database is locked")
}

// TestActiveModelName_NoChatModel: an agent with no usable chat model has no
// active model, and /status and /show model say "none".
func TestActiveModelName_NoChatModel(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Agents.List[0].Models = []string{"no-such-model"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	if got := al.activeModelName(agent, "s"); got != "" {
		t.Fatalf("activeModelName = %q, want none", got)
	}
	name, provider, protocol, apiBase := al.activeModelInfo(cfg, agent, &processOptions{SessionKey: "s"})
	if name != "none" || provider != "" || protocol != "" || apiBase != "" {
		t.Fatalf("activeModelInfo = %q %q %q %q, want none and nothing else", name, provider, protocol, apiBase)
	}
}

// TestSaveSelection_StoresOnlyAChatChoiceOfAKnownAgent: the selection store
// keeps a session's chat-model choice only; another purpose, a set with no
// running agent and an unreadable state store nothing and return no error
// (the catalogue keeps the choice in memory), and the unreadable state is
// logged.
func TestSaveSelection_StoresOnlyAChatChoiceOfAKnownAgent(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, agent := fourModelLoop(t)
	sel := sessionSelections{al: al}
	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		t.Fatalf("session store %T does not keep compaction state", agent.Sessions)
	}

	if err := sel.SaveSelection(agent.ID, models.Purpose("image"), "s", "a2"); err != nil {
		t.Fatalf("SaveSelection(image) = %v, want nil", err)
	}
	if err := sel.SaveSelection("nobody", models.Chat, "s", "a2"); err != nil {
		t.Fatalf("SaveSelection(unknown agent) = %v, want nil", err)
	}
	if st, err := store.GetCompactionState("s"); err != nil || st.ActiveModelIndex != 0 {
		t.Fatalf("state = %+v, %v; want no stored selection", st, err)
	}

	if err := sel.SaveSelection(agent.ID, models.Chat, "s", "a2"); err != nil {
		t.Fatal(err)
	}
	if st, err := store.GetCompactionState("s"); err != nil || st.ActiveModelIndex != 2 {
		t.Fatalf("state = %+v, %v; want ActiveModelIndex 2", st, err)
	}

	broken := &sessStateCovUnreadable{SessionStore: agent.Sessions}
	agent.Sessions = broken
	if err := sel.SaveSelection(agent.ID, models.Chat, "s", "a3"); err != nil {
		t.Fatalf("SaveSelection(unreadable) = %v, want nil", err)
	}
	if broken.sets != 0 {
		t.Fatalf("an unreadable state was written %d times, want none", broken.sets)
	}
	if !strings.Contains(logs.String(), "active model index: load compaction state failed") {
		t.Fatalf("the unreadable state was not logged:\n%s", logs.String())
	}
}

// TestSessionToggles_UnreadableStateKeepsMemoryOnly: /reasoning and /tools
// on a session whose state cannot be read take effect in memory, write
// nothing, and are logged.
func TestSessionToggles_UnreadableStateKeepsMemoryOnly(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, agent := fourModelLoop(t)
	broken := &sessStateCovUnreadable{SessionStore: agent.Sessions}
	agent.Sessions = broken

	al.setExposeReasoning(agent, "s", true)
	al.setShowToolActivity(agent, "s", true)
	if !al.getExposeReasoning(agent, "s") || !al.getShowToolActivity(agent, "s") {
		t.Fatal("the switches must take effect in memory")
	}
	if broken.sets != 0 {
		t.Fatalf("an unreadable state was written %d times, want none", broken.sets)
	}
	for _, want := range []string{"expose reasoning: load compaction state failed", "show tool activity: load compaction state failed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// sessStateCovCogmemAgent is a loop's default agent with cognitive memory
// on (the default) and its memory directory.
func sessStateCovCogmemAgent(t *testing.T) (*AgentLoop, *AgentInstance, string) {
	t.Helper()
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	if agent == nil || !agent.Config.CognitiveMemoryEnabled() {
		t.Fatal("the default agent must have cognitive memory")
	}
	return al, agent, cogmemstore.DBPath(cogmemhost.Dir(agent.StateDir))
}

// sessStateCovOpen opens the agent's memory store for the test to fill.
func sessStateCovOpen(t *testing.T, path string) *cogmemstore.Store {
	t.Helper()
	s, err := cogmemstore.Open(path)
	if err != nil {
		t.Fatalf("open cognitive memory: %v", err)
	}
	return s
}

// TestCogmemSessionStatus_NotCognitive: an agent without cognitive memory
// has no /memory status.
func TestCogmemSessionStatus_NotCognitive(t *testing.T) {
	al, agent, _ := sessStateCovCogmemAgent(t)
	if got := al.cogmemSessionStatus(t.Context(), nil, "s"); got != "" {
		t.Fatalf("nil agent: %q, want empty", got)
	}
	off := false
	cfgOff := *agent.Config
	cfgOff.Cogmem = &off
	plain := &AgentInstance{ID: agent.ID, StateDir: agent.StateDir, Config: &cfgOff}
	if got := al.cogmemSessionStatus(t.Context(), plain, "s"); got != "" {
		t.Fatalf("cogmem off: %q, want empty", got)
	}
	if got := al.cogmemSessionStatus(t.Context(), &AgentInstance{ID: "bare"}, "s"); got != "" {
		t.Fatalf("no config: %q, want empty", got)
	}
}

// TestCogmemSessionStatus_Counts: /memory counts the active domains and their
// active memories, and says when no consolidation has run yet.
func TestCogmemSessionStatus_Counts(t *testing.T) {
	al, agent, path := sessStateCovCogmemAgent(t)
	if got := al.cogmemSessionStatus(t.Context(), agent, "s"); got != "No cognitive-memory database for this assistant yet." {
		t.Fatalf("before any memory: %q", got)
	}

	ctx := context.Background()
	s := sessStateCovOpen(t, path)
	base, err := s.ListDomains(ctx, s.DB(), cogmemstore.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice-projects", "bob-travel"} {
		d, createErr := s.CreateDomain(ctx, s.DB(), cogmemstore.CreateDomainParams{Name: name, Status: cogmemstore.StatusActive, Summary: name})
		if createErr != nil {
			t.Fatalf("CreateDomain(%s): %v", name, createErr)
		}
		for _, text := range []string{"Alice prefers mornings", "Bob flies on Tuesdays"} {
			if _, addErr := s.AddMemory(ctx, s.DB(), cogmemstore.AddMemoryParams{
				DomainID: d.ID, Type: cogmemstore.TypeFact, Text: name + ": " + text, Confidence: 0.9, Origin: cogmemstore.OriginUser,
			}); addErr != nil {
				t.Fatalf("AddMemory: %v", addErr)
			}
		}
	}
	retired, err := s.AddMemory(ctx, s.DB(), cogmemstore.AddMemoryParams{
		DomainID: base[0].ID, Type: cogmemstore.TypeFact, Text: "retired note", Confidence: 0.5, Origin: cogmemstore.OriginUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RetireMemory(ctx, s.DB(), retired.ID, "outdated"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	want := "Active domains: " + strconv.Itoa(len(base)+2) + "\nActive memories: 4\nLast consolidation: none yet"
	if got := al.cogmemSessionStatus(t.Context(), agent, "s"); got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

// TestCogmemSessionStatus_LastRun: /memory names the last consolidation run,
// what triggered it, how it ended and how many changes it made, and the
// error of a failed run.
func TestCogmemSessionStatus_LastRun(t *testing.T) {
	for _, tc := range []struct {
		name, status, errText, wantTail string
	}{
		{"succeeded", "ok", "", "— trigger=nightly, status=ok, changes=4"},
		{"failed", "error", "model timed out", "— trigger=nightly, status=error, changes=4\nLast error: model timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, agent, path := sessStateCovCogmemAgent(t)
			ctx := context.Background()
			s := sessStateCovOpen(t, path)
			started := time.Date(2026, time.November, 22, 3, 15, 0, 0, time.UTC)
			if err := s.RecordRun(ctx, s.DB(), cogmemstore.Run{
				ID: "run-1", Trigger: "nightly", Model: "test-model", Status: tc.status,
				OpsApplied: 4, Error: tc.errText, StartedAt: started,
			}); err != nil {
				t.Fatal(err)
			}
			run, ok, err := s.LastRun(ctx, s.DB())
			if err != nil || !ok {
				t.Fatalf("LastRun: %v, %v", ok, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			got := al.cogmemSessionStatus(t.Context(), agent, "s")
			want := "Last consolidation: " + run.StartedAt.Format("2006-01-02 15:04") + " " + tc.wantTail
			if !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "Active domains: ") {
				t.Fatalf("status = %q, want it to end with %q", got, want)
			}
		})
	}
}

// TestCogmemSessionStatus_Unavailable: a memory database that cannot be
// opened or read gives "Cognitive memory unavailable" with the reason.
func TestCogmemSessionStatus_Unavailable(t *testing.T) {
	al, agent, path := sessStateCovCogmemAgent(t)
	if err := os.MkdirAll(path, 0o700); err != nil { // a directory where the database belongs
		t.Fatal(err)
	}
	if got := al.cogmemSessionStatus(t.Context(), agent, "s"); !strings.HasPrefix(got, "Cognitive memory unavailable: ") {
		t.Fatalf("status = %q, want the unavailable message", got)
	}
}
