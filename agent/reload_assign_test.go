// ClawEh
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

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/models"

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

// A build is tied to the reload whose builder made it: while that reload
// builds it reaches no catalogue; once it has committed, a build (a run-time
// creation) goes to the running catalogue; one made after it failed or was
// abandoned reaches none.
func TestAssignBuilt_TiedToItsReload(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := abcConfig(t.TempDir(), "a", "b", "c")
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	newAgent := func(id string) *AgentInstance {
		return &AgentInstance{ID: id, chatModels: []string{"b", "a"}, Model: "b", HumanModel: ""}
	}
	listOf := func(id string) []string { return entryNames(al.catalog.Set(id).List(models.Chat)) }

	building := &reloadAssignments{cfg: cfg}
	al.assignBuilt(building, cfg, newAgent("x"))
	if got := listOf("x"); len(got) != 0 {
		t.Fatalf("a build of a reload still building reached the catalogue: %v", got)
	}
	if built := building.end(true); len(built) != 1 {
		t.Fatalf("the reload recorded %d builds, want 1", len(built))
	}
	al.assignBuilt(building, cfg, newAgent("y"))
	if got := listOf("y"); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("a run-time build after the commit = %v, want [b a]", got)
	}

	failed := &reloadAssignments{cfg: cfg}
	failed.end(false)
	al.assignBuilt(failed, cfg, newAgent("z"))
	if got := listOf("z"); len(got) != 0 {
		t.Fatalf("a late build of a failed reload reached the catalogue: %v", got)
	}

	al.assignBuilt(nil, cfg, newAgent("w"))
	if got := listOf("w"); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("a build of the first builder = %v, want [b a]", got)
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
	// With the set emptied (the catalogue left the models out), the first
	// resolved name still names it.
	if _, err := al.catalog.Set(agent.ID).Replace(models.Chat); err != nil {
		t.Fatal(err)
	}
	if got := al.activeModelName(agent, "s"); got != "b" {
		t.Fatalf("active model with no set = %q, want b", got)
	}
}

// An agent none of whose models resolves runs, as it always has, on the
// agents' default model's client with its first configured name as the
// model and its own limits: one call, one finish line with no provider and
// that name, nothing parked.
func TestUnresolvedAgent_RunsOnDefaultModelClient(t *testing.T) {
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"no-such-model"}})
	p := &replayProvider{errs: map[string]string{}}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	if err := al.catalog.RegisterClient("a", p); err != nil {
		t.Fatal(err)
	}
	bob, _ := al.GetRegistry().Get("bob")

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	tm := al.selectTurnModel(bob, "s")
	if len(tm.candidates) != 0 || tm.direct != "a" || tm.model != "no-such-model" {
		t.Fatalf("turn model = %+v", tm)
	}
	if tm.limits.maxTokens != cfg.Agents.Defaults.MaxTokens {
		t.Errorf("max tokens = %d, want the defaults' %d", tm.limits.maxTokens, cfg.Agents.Defaults.MaxTokens)
	}
	turn := &llmTurn{
		al: al, agent: bob, chain: tm.chain, candidates: tm.candidates, model: tm.model, limits: tm.limits, direct: tm.direct,
		messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	if _, err := turn.callModel(context.Background(), turn.newDispatch(nil, 0)); err != nil {
		t.Fatal(err)
	}
	if got := p.calls(); !slices.Equal(got, []string{"no-such-model"}) {
		t.Fatalf("calls = %v, want the configured name on the default model's client", got)
	}
	fin := finishRecords(t, buf)
	if len(fin) != 1 {
		t.Fatalf("finish lines = %d, want 1", len(fin))
	}
	assertFields(t, fin[0], map[string]any{"our_provider": "", "our_model": "no-such-model", "agent_id": "bob"})
}

// A summarization name that is no configured model runs on the default
// model's client under that name; its failures are kept and alerted under
// the key the summarization chain always used (no provider, the name), and
// /cooldowns shows it so.
func TestDerivedSummaryModel_KeyAndCooldowns(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Summarization.Models = []string{"raw-summarizer"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	main := al.GetRegistry().Default()
	name := providers.DerivedSummaryName("raw-summarizer")
	if got := entryNames(al.catalog.Set(main.ID).List(models.Summarize)); !slices.Equal(got, []string{name, "a"}) {
		t.Fatalf("summarize list = %q", got)
	}
	if got := al.chatModelNames(main); slices.Contains(got, name) {
		t.Fatalf("/model lists the derived model: %v", got)
	}
	failing := &scriptedProvider{err: errors.New("API request failed:   Status: 402   Body: {}")}
	if err := al.catalog.RegisterClient(name, failing); err != nil {
		t.Fatal(err)
	}
	c, _ := al.newCompressModelCaller(main, "s", models.Summarize)
	if _, err := c.Complete(context.Background(), ctxRequest()); err != nil {
		t.Fatalf("Complete: %v (a should have answered)", err)
	}
	if len(rec.alerts) != 1 || rec.alerts[0].EventID != "/raw-summarizer" {
		t.Fatalf("alerts = %+v, want one keyed /raw-summarizer", rec.alerts)
	}
	rt := newCooldownRuntime(al)
	got := rt.ListCooldowns()
	if len(got) != 1 || got[0].Provider != "" || got[0].Model != "raw-summarizer" {
		t.Fatalf("/cooldowns = %+v, want /raw-summarizer", got)
	}
	if !rt.ClearCooldown("", "raw-summarizer") || len(rt.ListCooldowns()) != 0 {
		t.Fatal("/cooldowns clear /raw-summarizer did not clear it")
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
	}
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
	if _, _, err := host.Complete(context.Background(), "alice", "s", "u"); err == nil {
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
	cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: "orphan", Model: "o", Provider: "nobody", Enabled: true})
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
func ctxRequest() ctxengine.ModelRequest { return ctxengine.ModelRequest{System: "s", User: "u"} }

// newCooldownRuntime is a command runtime with al's cooldown hooks.
func newCooldownRuntime(al *AgentLoop) *commands.Runtime {
	rt := &commands.Runtime{}
	al.addCooldownHooks(rt)
	return rt
}
