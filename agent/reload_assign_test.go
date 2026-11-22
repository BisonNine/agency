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
	"sync"
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
	// With the set's chat list emptied (the catalogue left the models out),
	// the turn runs on the default model's client under the configured name,
	// and status names what is sent.
	if _, err := al.catalog.Set(agent.ID).Replace(models.Chat); err != nil {
		t.Fatal(err)
	}
	if got, turn := al.activeModelName(agent, "s"), al.selectTurnModel(agent, "s").model; got != "b-wire" || turn != got {
		t.Fatalf("active model with no set = %q, the turn's %q; want b-wire for both", got, turn)
	}
}

// A busy temporary agent outlives a reload that removed its models: its
// next turn runs on the new default model's client under the agent's own
// configured name, and status names that same name.
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
	changed.Models = slices.DeleteFunc(changed.Models, func(m config.ModelConfig) bool { return m.ModelName == "a" })
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
	if len(tm.candidates) != 1 || tm.candidates[0].Name != now.Model || tm.candidates[0].Provider != "" || tm.model != now.Model {
		t.Fatalf("turn model = %+v, want %q on b's client", tm, now.Model)
	}
	if got := al.activeModelName(now, "s"); got != now.Model {
		t.Fatalf("active model = %q, want %q", got, now.Model)
	}
	turn := hostTurn(t, al, now, processOptions{}, tm, providers.Message{Role: "user", Content: "hi"})
	if _, err := turn.callModel(context.Background(), newTestDispatch(turn)); err != nil {
		t.Fatal(err)
	}
	if got := p.calls(); !slices.Equal(got, []string{now.Model}) {
		t.Fatalf("calls = %v, want [%s]", got, now.Model)
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
	if len(tm.candidates) != 1 || tm.candidates[0].Name != "no-such-model" || tm.candidates[0].Provider != "" || tm.model != "no-such-model" {
		t.Fatalf("turn model = %+v", tm)
	}
	if tm.limits.maxTokens != cfg.Agents.Defaults.MaxTokens {
		t.Errorf("max tokens = %d, want the defaults' %d", tm.limits.maxTokens, cfg.Agents.Defaults.MaxTokens)
	}
	turn := hostTurn(t, al, bob, processOptions{}, tm, providers.Message{Role: "user", Content: "hi"})
	if _, err := turn.callModel(context.Background(), newTestDispatch(turn)); err != nil {
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

// A derived summarization model follows the default model: change only the
// default model and the next summary call for the raw name goes to the new
// default's provider and protocol; drop the raw name and no model or
// /cooldowns entry is left for it.
func TestDerivedSummaryModel_FollowsDefaultModel(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	conf := func(def string, summarizers ...string) *config.Config {
		cfg := abcConfig(base, def)
		cfg.Providers = append(cfg.Providers, config.Provider{Name: "q", Protocol: "anthropic", BaseURL: "http://127.0.0.1:0", APIKey: "k"})
		cfg.Models[2].Provider = "q" // c
		cfg.Summarization.Models = summarizers
		return cfg
	}
	al := mustNewAgentLoop(t, conf("a", "raw-summarizer"), bus.NewMessageBus(), &mockProvider{}, nil)
	name := providers.DerivedSummaryName("raw-summarizer")
	first := func() models.Candidate {
		t.Helper()
		cands := al.catalog.Set("main").Begin(models.Summarize, "s").Candidates()
		if len(cands) == 0 || cands[0].Name != name {
			t.Fatalf("summarize candidates = %+v, want %s first", cands, name)
		}
		return cands[0]
	}
	if c := first(); c.Provider != "p" || c.Protocol != "openai-chat" || c.Model != "raw-summarizer" {
		t.Fatalf("before = %s/%s/%s", c.Provider, c.Protocol, c.Model)
	}
	if err := al.catalog.RegisterClient(name, &scriptedProvider{err: errors.New("API request failed:   Status: 402   Body: {}")}); err != nil {
		t.Fatal(err)
	}
	caller, _ := al.newCompressModelCaller(al.GetRegistry().Default(), "s", models.Summarize)
	if _, err := caller.Complete(context.Background(), ctxRequest()); err != nil {
		t.Fatalf("Complete: %v (a should have answered)", err)
	}
	if len(newCooldownRuntime(al).ListCooldowns()) == 0 {
		t.Fatal("the failing derived model was not parked")
	}

	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, conf("c", "raw-summarizer")); err != nil {
		t.Fatal(err)
	}
	if c := first(); c.Provider != "q" || c.Protocol != "anthropic" || c.Model != "raw-summarizer" {
		t.Fatalf("after the default model changed = %s/%s/%s, want q/anthropic/raw-summarizer", c.Provider, c.Protocol, c.Model)
	}

	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, conf("c")); err != nil {
		t.Fatal(err)
	}
	if _, ok := al.catalog.Get(name); ok {
		t.Fatal("the derived model of a dropped name remains")
	}
	for _, cd := range newCooldownRuntime(al).ListCooldowns() {
		if cd.Model == "raw-summarizer" {
			t.Fatalf("/cooldowns lists the dropped name: %+v", cd)
		}
	}
}

// A busy temporary agent built while a derived summarization model could be
// made does not bring it back once the configuration in force can no longer
// make it (no default model).
func TestDerivedSummaryModel_NotRestoredFromOldConfig(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	base := t.TempDir()
	conf := func(def string) *config.Config {
		cfg := abcConfig(base, def)
		cfg.Summarization.Models = []string{"raw-summarizer"}
		return cfg
	}
	al := mustNewAgentLoop(t, conf("a"), bus.NewMessageBus(), &mockProvider{}, nil, OwnsDataDir())
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
	name := providers.DerivedSummaryName("raw-summarizer")
	if _, ok := al.catalog.Get(name); !ok {
		t.Fatal("no derived model to begin with")
	}
	if err := reloadForTest(t, al, context.Background(), &mockProvider{}, conf("no-such-model")); err != nil {
		t.Fatal(err)
	}
	if now, _ := r.Get(id); now != clone {
		t.Fatal("the busy clone was rebuilt")
	}
	if _, ok := al.catalog.Get(name); ok {
		t.Fatal("the derived model came back from the busy clone's configuration")
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

// optsRecorder records the options of each call; cli makes it a CLI client.
type optsRecorder struct {
	mu   sync.Mutex
	opts []map[string]any
	cli  bool
}

func (p *optsRecorder) Chat(_ context.Context, _ providers.Identity, _ []providers.Message, _ []providers.ToolDefinition, _ string, opts map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = append(p.opts, opts)
	return &providers.LLMResponse{Content: "ok"}, nil
}
func (p *optsRecorder) GetDefaultModel() string { return "" }
func (p *optsRecorder) IsCLI() bool             { return p.cli }

// The direct path sends a temperature to an HTTP default model and none to
// a CLI one, as the default provider's own path did.
func TestUnresolvedAgent_DefaultModelTemperature(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	temp := 0.3
	for _, tc := range []struct {
		name     string
		protocol string
		cli      bool
		want     bool
	}{
		{"http", "openai-chat", false, true},
		{"cli", "claude-cli", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := abcConfig(t.TempDir(), "a")
			cfg.Providers[0].Protocol = tc.protocol
			cfg.Agents.Defaults.Temperature = &temp
			cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"nope"}})
			al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
			p := &optsRecorder{cli: tc.cli}
			if err := al.catalog.RegisterClient("a", p); err != nil {
				t.Fatal(err)
			}
			bob, _ := al.GetRegistry().Get("bob")
			tm := al.selectTurnModel(bob, "s")
			turn := hostTurn(t, al, bob, processOptions{}, tm, providers.Message{Role: "user", Content: "hi"})
			if _, err := turn.callModel(context.Background(), newTestDispatch(turn)); err != nil {
				t.Fatal(err)
			}
			if len(p.opts) != 1 {
				t.Fatalf("calls = %d", len(p.opts))
			}
			if _, got := p.opts[0]["temperature"]; got != tc.want {
				t.Fatalf("temperature sent = %v, want %v", got, tc.want)
			}
		})
	}
}
