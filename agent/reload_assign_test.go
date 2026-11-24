// Agency
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// A temporary agent in a turn while a reload adopts a new catalogue (the
// reload does not rebuild it) keeps its models in the new catalogue and its
// session's /model choice, in memory and on disk; its next turn runs.
func TestReloadAdopt_BusyTemporaryAgentKeepsModels(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	cfg := abcConfig(base, "a", "b", "c")
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	r := al.GetRegistry()
	id, err := r.CreateClone("main")
	if err != nil {
		t.Fatal(err)
	}
	clone, _ := r.Get(id)
	const session = "s"
	selectModel(t, al, clone, session, "c")
	end, ok := r.BeginTurn(id, clone)
	if !ok {
		t.Fatal("the clone could not begin a turn")
	}

	changed := abcConfig(base, "a", "b", "c")
	changed.Models[0].RequestTimeout = 77 // a model setting: a new catalogue
	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, changed); err != nil {
		t.Fatal(err)
	}
	end()
	if info, _ := al.catalog.Get("a"); info.Model.RequestTimeout != 77 {
		t.Fatal("the reload did not adopt a new catalogue")
	}
	now, _ := r.Get(id)
	if got := al.chatModelNames(now); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("the busy clone's list after the reload = %v", got)
	}
	if got := al.activeModelName(now, session); got != "c" {
		t.Fatalf("the busy clone's choice after the reload = %q, want c", got)
	}
	store, ok := now.Sessions.(compactionStateStore)
	if !ok {
		t.Fatal("no compaction state store")
	}
	if st, err := store.GetCompactionState(session); err != nil || st.ActiveModelIndex != 2 {
		t.Fatalf("stored index = %d, %v; want 2 (c) kept", st.ActiveModelIndex, err)
	}
	if tm := al.selectTurnModel(now, session); tm.model != "c" || len(tm.candidates) != 3 {
		t.Fatalf("the clone's next turn = %q on %d models", tm.model, len(tm.candidates))
	}
}

// The turn starts on the first model the agent's list resolves to, not on
// the raw name the list begins with.
func TestActiveModelName_FirstResolvedModel(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := abcConfig(t.TempDir(), "b-wire", "a")
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.GetRegistry().Default()
	if got := al.activeModelName(agent, "s"); got != "b" {
		t.Fatalf("active model = %q, want b (what b-wire resolves to)", got)
	}
	// With the set's chat list emptied (the catalogue left the models out)
	// there is no model: status names none, and the turn has none to call.
	if _, err := al.catalog.Set(agent.ID).Replace(models.Chat); err != nil {
		t.Fatal(err)
	}
	if got, tm := al.activeModelName(agent, "s"), al.selectTurnModel(agent, "s"); got != "" || tm.model != "" || len(tm.candidates) != 0 {
		t.Fatalf("active model with no set = %q, the turn's %q on %d models; want none", got, tm.model, len(tm.candidates))
	}
}

// A busy temporary agent outlives a reload that removed its models: its
// next turn fails with no model configured, calling no model (not the new
// default model either), and status names no model.
func TestReloadAdopt_BusyTemporaryAgentModelsRemoved(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a-wire"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	r := al.GetRegistry()
	id, err := r.CreateClone("main")
	if err != nil {
		t.Fatal(err)
	}
	clone, _ := r.Get(id)
	end, ok := r.BeginTurn(id, clone)
	if !ok {
		t.Fatal("the clone could not begin a turn")
	}
	defer end()

	changed := abcConfig(base, "b")
	changed.Models = slices.DeleteFunc(changed.Models, func(m modelspec.Model) bool { return m.ModelName == "a" })
	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, changed); err != nil {
		t.Fatal(err)
	}
	if _, ok := al.catalog.Get("a"); ok {
		t.Fatal("the reload kept model a")
	}
	p := &replayProvider{errs: map[string]string{}}
	if err := al.catalog.RegisterClient("b", p); err != nil {
		t.Fatal(err)
	}
	now, _ := r.Get(id)
	if now.Model != "a-wire" {
		t.Fatalf("the clone's configured model = %q, want a-wire", now.Model)
	}
	tm := al.selectTurnModel(now, "s")
	if len(tm.candidates) != 0 || tm.model != "" {
		t.Fatalf("turn model = %+v, want none", tm)
	}
	if got := al.activeModelName(now, "s"); got != "" {
		t.Fatalf("active model = %q, want none", got)
	}
	if _, _, err := testTurn(t, al, now, processOptions{SessionKey: "s"}, tm, nil); !errors.Is(err, errNoModelConfigured) {
		t.Fatalf("err = %v, want errNoModelConfigured", err)
	}
	if got := p.calls(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// Deleting temporary agents removes their sets.
func TestTemporaryAgentDeleted_SetRemoved(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, abcConfig(t.TempDir(), "a", "b"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	r := al.GetRegistry()
	for range 5 {
		id, err := r.CreateClone("main")
		if err != nil {
			t.Fatal(err)
		}
		if len(al.catalog.Set(id).Purposes()) == 0 {
			t.Fatal("the clone has no models")
		}
		if err := r.Delete(id); err != nil {
			t.Fatal(err)
		}
		if p := al.catalog.Set(id).Purposes(); len(p) != 0 {
			t.Fatalf("the deleted clone's set remains: %v", p)
		}
		if n := al.liveCount(id); n != 0 {
			t.Fatalf("%d unretired instance(s) of the deleted clone remain", n)
		}
	}
	noUnretiredLeak(t, al)
}

// A temporary agent deleted after its insertion but before its lists are
// stated gets no set and leaves no bookkeeping.
func TestAssignOne_SkipsAgentDeletedBeforeLists(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, abcConfig(t.TempDir(), "a", "b"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	r := al.GetRegistry()
	// The insert hook waits on listsGate; the deletion does not.
	al.listsGate.Lock()
	created := make(chan error, 1)
	go func() {
		_, err := r.CreateClone("main")
		created <- err
	}()
	var id string
	waitFor(t, "the clone's insertion", func() bool {
		for _, cand := range r.All() {
			if cand != "main" {
				id = cand
				return true
			}
		}
		return false
	})
	if err := r.Delete(id); err != nil {
		t.Fatal(err)
	}
	al.listsGate.Unlock()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if p := al.catalog.Set(id).Purposes(); len(p) != 0 {
		t.Fatalf("the deleted clone has a set: %v", p)
	}
	if n := al.liveCount(id); n != 0 {
		t.Fatalf("%d unretired instance(s) of the deleted clone", n)
	}
	noUnretiredLeak(t, al)
}

// A configured agent dropped from the configuration loses its set; one that
// stays keeps it.
func TestConfiguredAgentDropped_SetRemoved(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	withBob := func() *config.Config {
		cfg := abcConfig(base, "a", "b")
		cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob"})
		return cfg
	}
	al := mustNewAgentLoop(t, withBob(), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
	if len(al.catalog.Set("bob").Purposes()) == 0 {
		t.Fatal("bob has no models")
	}
	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, abcConfig(base, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if _, ok := al.GetRegistry().Get("bob"); ok {
		t.Fatal("bob is still registered")
	}
	if p := al.catalog.Set("bob").Purposes(); len(p) != 0 {
		t.Fatalf("the dropped agent's set remains: %v", p)
	}
	if len(al.catalog.Set("main").Purposes()) == 0 {
		t.Fatal("main lost its models")
	}
	if n := al.liveCount("bob"); n != 0 {
		t.Fatalf("%d unretired instance(s) of bob remain", n)
	}
	noUnretiredLeak(t, al)
}

// A settings change that clears the cooldowns re-arms the alerts, as an
// operator's clear does: a model alerted for an auth failure that still
// fails alerts again at once.
func TestAdopt_RearmsAlerts(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a"), bus.NewMessageBus(), &mockProvider{}, nil)
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	fail := func() {
		t.Helper()
		if err := al.catalog.RegisterClient("a", &scriptedProvider{err: errors.New("API request failed:   Status: 401   Body: {}")}); err != nil {
			t.Fatal(err)
		}
		_, _ = providers.ModelSet(al.catalog, "a").Begin(models.Chat).Chat(context.Background(), testIdentity, models.Request{}) //nolint:errcheck // it fails by design
	}
	fail()
	if len(rec.alerts) != 1 {
		t.Fatalf("alerts = %d, want the auth alert", len(rec.alerts))
	}
	changed := abcConfig(base, "a")
	changed.Models[0].RequestTimeout = 5
	if err := al.ReloadConfig(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	fail()
	if len(rec.alerts) != 2 || !strings.Contains(rec.alerts[1].Description, "ongoing since") {
		t.Fatalf("alerts after the settings change = %+v, want the auth alert again", rec.alerts)
	}
}

// A classifier request is made once: a Retry-After of a second is not
// waited out for a second, billed call.
func TestClassifierHost_NoSameModelRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	_, host, _ := newClassifierHostLoop(t, classifierHostConfig(t, srv.URL))
	start := time.Now()
	if _, _, err := host.Complete(context.Background(), "alice", "", "s", "u"); err == nil {
		t.Fatal("Complete succeeded on a 429")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatal("the Retry-After was waited out")
	}
}

// CheckDefaultModel builds its catalogue quietly: it logs nothing the
// running catalogue's build logs.
func TestCheckDefaultModel_Quiet(t *testing.T) {
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "orphan", Model: "o", Provider: "nobody", Enabled: true})
	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if err := providers.CheckDefaultModel(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("the check logged: %s", buf.String())
	}
}

// ctxRequest is a summarization request.
func ctxRequest() hook.CompleteRequest { return hook.CompleteRequest{System: "s", User: "u"} }

// newCooldownRuntime is a command runtime with al's cooldown hooks.
func newCooldownRuntime(al *AgentLoop) *commands.Runtime {
	rt := &commands.Runtime{}
	al.addCooldownHooks(rt)
	return rt
}
