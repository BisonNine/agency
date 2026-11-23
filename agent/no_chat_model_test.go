// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// noModelReply is what a person gets for a turn of an agent with no model.
const noModelReply = "Error processing message: LLM call failed after retries: no model configured — enable a model and add your API key in the configuration"

// An agent none of whose models is in the model list and enabled (an
// unknown name, a disabled model, an empty name) does not run on the
// agents' default model: its turn fails with no model configured, no model
// is called, and the person gets the error reply with an error outcome.
func TestNoChatModel_TurnFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []string
	}{
		{"unknown model", []string{"no-such-model"}},
		{"disabled model", []string{"off"}},
		{"empty name", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
			cfg := newTestConfig(t)
			cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: "off", Model: "off", Provider: testProviderName, Enabled: false})
			cfg.Agents.List[0].Models = tc.models
			msgBus := bus.NewMessageBus()
			p := &replayProvider{errs: map[string]string{}}
			al := mustNewAgentLoop(t, cfg, msgBus, p, nil)
			main := al.GetRegistry().Default()
			if got := al.chatModelNames(main); len(got) != 0 {
				t.Fatalf("chat models = %v, want none", got)
			}

			dispatch(al, inbound("c1", "id1", "hi"))
			out := nextOutbound(t, msgBus)
			if out.Outcome != bus.OutcomeError || out.Content != noModelReply {
				t.Fatalf("reply = %q (outcome %q), want %q with an error outcome", out.Content, out.Outcome, noModelReply)
			}
			if got := p.calls(); len(got) != 0 {
				t.Fatalf("model calls = %q, want none (the default model is not used)", got)
			}
		})
	}
}

// The turn of an agent with no model fails before any call: its chain has
// no candidates, callModel returns errNoModelConfigured, and the turn's
// finish line names no model.
func TestNoChatModel_NoCandidates(t *testing.T) {
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"no-such-model"}})
	p := &replayProvider{errs: map[string]string{}}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), p, nil)
	bob, _ := al.GetRegistry().Get("bob")

	tm := al.selectTurnModel(bob, "s")
	if len(tm.candidates) != 0 || tm.model != "" {
		t.Fatalf("turn model = %+v, want none", tm)
	}
	if got := al.activeModelName(bob, "s"); got != "" {
		t.Fatalf("active model = %q, want none", got)
	}
	// /status and /show model say there is none.
	if name, provider, _, _ := al.activeModelInfo(cfg, bob, &processOptions{SessionKey: "s"}); name != "none" || provider != "" {
		t.Fatalf("model info = %q (%q), want none", name, provider)
	}
	if _, _, err := testTurn(t, al, bob, processOptions{SessionKey: "s"}, tm, nil); !errors.Is(err, errNoModelConfigured) {
		t.Fatalf("err = %v, want errNoModelConfigured", err)
	}
	if got := p.calls(); len(got) != 0 {
		t.Fatalf("model calls = %q, want none", got)
	}
}

// A summarization model that is not in the model list, or is disabled, is
// skipped and said so: no model stands in for it, the agent's primary chat
// model serves, and the catalogue gains no model.
func TestSummarization_UnknownEntrySkipped(t *testing.T) {
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Models[2].Enabled = false // c
	cfg.Summarization.Models = []string{"raw-summarizer", "c"}
	p := &replayProvider{errs: map[string]string{}}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), p, nil)
	main := al.GetRegistry().Default()
	if got := entryNames(al.catalog.Set(main.ID).List(models.Summarize)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("summarize list = %q, want [a]", got)
	}
	if got := entryNames(al.catalog.Set(main.ID).List(models.Memory)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("memory list = %q, want [a]", got)
	}
	if got := al.catalog.Names(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"a", "b"}) {
		t.Fatalf("catalogue models = %q, want [a b]", got)
	}

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	c, _ := al.newCompressModelCaller(main, "s", models.Summarize)
	if _, err := c.Complete(context.Background(), ctxRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := p.calls(); !slices.Equal(got, []string{"a-wire"}) {
		t.Fatalf("calls = %q, want the primary chat model only", got)
	}
	var skipped []any
	for _, rec := range readLogRecords(t, buf) {
		if rec["message"] == "compression model not found in enabled models; skipping" {
			skipped = append(skipped, rec["requested"])
		}
	}
	if !slices.Equal(skipped, []any{"raw-summarizer", "c"}) {
		t.Fatalf("skipping lines name %v, want raw-summarizer and c", skipped)
	}
	if strings.Contains(buf.String(), "falling back") {
		t.Fatalf("a fallback is logged:\n%s", buf.String())
	}
}

// The primary chat model appended to the summarization chain is the
// agent's first chat model in the model list and enabled, not a model
// standing in for a first entry that is neither.
func TestSummarization_PrimaryIsFirstChatModel(t *testing.T) {
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Models[2].Enabled = false // c
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"c", "b"}})
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	for _, p := range []models.Purpose{models.Summarize, models.Memory} {
		if got := entryNames(al.catalog.Set("bob").List(p)); !slices.Equal(got, []string{"b"}) {
			t.Fatalf("%s list = %q, want [b]", p, got)
		}
	}

	// With no chat model there is no primary: the summarization lists
	// hold only the summarization models that are in the model list.
	cfg = abcConfig(t.TempDir(), "a")
	cfg.Summarization.Models = []string{"b"}
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", Models: []string{"no-such-model"}})
	al = mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	if got := entryNames(al.catalog.Set("bob").List(models.Summarize)); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("summarize list = %q, want [b]", got)
	}
}

// A summarization model that fails is parked, alerted and listed by
// /cooldowns under its own provider and model id, like any other model,
// and /cooldowns clear clears it.
func TestSummarization_CooldownUnderOwnKey(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := abcConfig(t.TempDir(), "a")
	cfg.Summarization.Models = []string{"b"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	if err := al.catalog.RegisterClient("b", &scriptedProvider{err: errors.New("API request failed:   Status: 402   Body: {}")}); err != nil {
		t.Fatal(err)
	}
	c, _ := al.newCompressModelCaller(al.GetRegistry().Default(), "s", models.Summarize)
	if _, err := c.Complete(context.Background(), ctxRequest()); err != nil {
		t.Fatalf("Complete: %v (a should have answered)", err)
	}
	if len(rec.alerts) != 1 || rec.alerts[0].EventID != spawnllm.ModelKey("p", "b-wire") {
		t.Fatalf("alerts = %+v, want one keyed by b's provider and model id", rec.alerts)
	}
	rt := newCooldownRuntime(al)
	got := rt.ListCooldowns()
	if len(got) != 1 || got[0].Provider != "p" || got[0].Model != "b-wire" {
		t.Fatalf("/cooldowns = %+v, want p/b-wire", got)
	}
	if !rt.ClearCooldown("p", "b-wire") || len(rt.ListCooldowns()) != 0 {
		t.Fatal("/cooldowns clear p/b-wire did not clear it")
	}
}
