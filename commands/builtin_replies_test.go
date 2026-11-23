package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine"

	"go.bison9.com/agency/app"
)

// reply runs text through the builtin commands with rt and returns the reply.
func reply(t *testing.T, rt *Runtime, text string) string {
	t.Helper()
	var got string
	res := NewExecutor(NewRegistry(BuiltinDefinitions()), rt).Execute(context.Background(), Request{
		Channel: "telegram",
		Text:    text,
		Reply:   func(s string) error { got = s; return nil },
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("%q was not handled", text)
	}
	return got
}

func TestBuiltinReplies(t *testing.T) {
	var cleared, reset, retriggered bool
	var switched string
	full := &Runtime{
		AgentName:      "Alice",
		SwitchChannel:  func(v string) error { switched = v; return nil },
		ClearHistory:   func() error { cleared = true; return nil },
		CancelPending:  func(bool) CancelOutcome { return CancelOutcome{Skipped: 2} },
		CompactHistory: func(context.Context) (string, error) { return "", nil },
		ResetCooldown:  func() { reset = true },
		RetriggerLastMessage: func(context.Context) error {
			retriggered = true
			return nil
		},
		GetModelInfo:       func() (string, string, string, string) { return "gpt", "openai", "", "" },
		ListAgentIDs:       func() []string { return []string{"alice", "bob"} },
		GetEnabledChannels: func() []string { return nil },
		GetAgentModels:     func() ([]ModelEntry, int) { return nil, 0 },
		ListDefinitions: func() []Definition {
			return []Definition{{Name: "x"}, {Name: "y", Usage: "/y <z>", Description: "does y"}}
		},
	}
	failing := &Runtime{
		SwitchChannel:        func(string) error { return errors.New("channel 'irc' is not enabled") },
		ClearHistory:         func() error { return errors.New("disk full") },
		CompactHistory:       func(context.Context) (string, error) { return "", errors.New("model down") },
		ResetCooldown:        func() {},
		RetriggerLastMessage: func(context.Context) error { return errors.New("no last message") },
		ListAgentIDs:         func() []string { return nil },
	}

	tests := []struct {
		name string
		rt   *Runtime
		text string
		want string
	}{
		{"check no runtime", nil, "/check channel irc", unavailableMsg},
		{"check usage", full, "/check channel", "Usage: /check channel <name>"},
		{"check ok", full, "/check channel slack", "Channel 'slack' is available and enabled"},
		{"check fails", failing, "/check channel irc", "channel 'irc' is not enabled"},
		{"clear no runtime", nil, "/clear", unavailableMsg},
		{"clear ok", full, "/clear", "Context cleared, but long-term memory preserved. (2 pending message(s) cancelled)"},
		{"clear alias fails", failing, "/reset", "Failed to clear chat history: disk full"},
		{"compact no runtime", nil, "/compact", unavailableMsg},
		{"compact ok", full, "/compact", "Conversation history compacted."},
		{"compact fails", failing, "/compact", "Failed to compact history: model down"},
		{"retry no runtime", nil, "/retry", unavailableMsg},
		{"retry fails", failing, "/retry", "Cooldowns cleared, but could not retrigger: no last message"},
		{"show model no runtime", nil, "/show model", unavailableMsg},
		{"show model", full, "/show model", "Current Model: gpt (Provider: openai)"},
		{"show agents", full, "/show agents", "Registered agents: alice, bob"},
		{"list agents none", failing, "/list agents", "No agents registered"},
		{"list agents no runtime", nil, "/list agents", unavailableMsg},
		{"list channels none", full, "/list channels", "No channels enabled"},
		{"list channels no runtime", nil, "/list channels", unavailableMsg},
		{"list models no runtime", nil, "/list models", noModelText(nil)},
		{"list models empty", full, "/list models", "Alice has no usable model: its models are not in the model list or disabled."},
		{"start named", full, "/start", "Hello! I am Alice."},
		{"start anonymous", nil, "/start", "Hello!"},
		{"version", nil, "/version", app.Name() + " " + app.Version() + "\n" + app.TagLine() + "\n" + app.Copyright()},
		{"help from runtime", full, "/help", "/x - No description\n/y <z> - does y"},
		{"model no runtime", nil, "/model 1", noModelText(nil)},
		{"model no entries", &Runtime{GetAgentModels: full.GetAgentModels, SetActiveModel: func(int) (string, error) { return "", nil }}, "/model 1", noModelText(nil)},
		{"cooldowns clear no runtime", nil, "/cooldowns clear", unavailableMsg},
		{"cooldowns clear all unavailable", &Runtime{}, "/cooldowns clear", unavailableMsg},
		{"cooldowns clear one unavailable", &Runtime{}, "/cooldowns clear openai/gpt", unavailableMsg},
		{"cooldowns list no runtime", nil, "/cooldowns list", unavailableMsg},
		{"quota reset no runtime", nil, "/quota reset", unavailableMsg},
		{"unknown sub-command", full, "/show nonsense", "Unknown option: nonsense. Usage: " + showCommand().EffectiveUsage()},
		{"sub-command usage", full, "/show", "Usage: " + showCommand().EffectiveUsage()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reply(t, tt.rt, tt.text); got != tt.want {
				t.Errorf("reply = %q, want %q", got, tt.want)
			}
		})
	}

	if switched != "slack" || !cleared {
		t.Errorf("switched=%q cleared=%v", switched, cleared)
	}

	// A successful /retry sends no reply of its own: the resent message
	// produces one.
	if got := reply(t, full, "/retry"); got != "" || !reset || !retriggered {
		t.Errorf("retry reply=%q reset=%v retriggered=%v", got, reset, retriggered)
	}
}

func TestCompactReplies(t *testing.T) {
	tests := []struct {
		report string
		err    error
		want   string
	}{
		{"Compacted 10 messages.", errors.New("ignored"), "Compacted 10 messages."},
		{"", ctxengine.ErrNothingToCompress, "Already compact — nothing to summarize."},
		{"", errors.Join(errors.New("wrap"), ctxengine.ErrNothingToCompress), "Already compact — nothing to summarize."},
	}
	for _, tt := range tests {
		rt := &Runtime{CompactHistory: func(context.Context) (string, error) { return tt.report, tt.err }}
		if got := reply(t, rt, "/compact"); got != tt.want {
			t.Errorf("reply = %q, want %q", got, tt.want)
		}
	}
}

func TestClearWithoutPendingNote(t *testing.T) {
	rt := &Runtime{ClearHistory: func() error { return nil }, CancelPending: func(bool) CancelOutcome { return CancelOutcome{} }}
	if got := reply(t, rt, "/clear"); got != "Context cleared, but long-term memory preserved." {
		t.Errorf("reply = %q", got)
	}
}

func TestHelpDefaults(t *testing.T) {
	got := reply(t, nil, "/help")
	if !strings.Contains(got, "/help - Show this help message") {
		t.Errorf("help without runtime does not list the builtins: %q", got)
	}
	if formatHelpMessage(nil) != "No commands available." {
		t.Error("empty help")
	}
}

func TestReasoningOnOff(t *testing.T) {
	on := false
	rt := &Runtime{GetExposeReasoning: func() bool { return on }, SetExposeReasoning: func(v bool) { on = v }}
	if got := reply(t, rt, "/reasoning"); !strings.HasPrefix(got, "Reasoning is currently OFF.") {
		t.Errorf("reply = %q", got)
	}
	reply(t, rt, "/reasoning on")
	if got := reply(t, rt, "/reasoning"); !strings.HasPrefix(got, "Reasoning is currently ON.") {
		t.Errorf("reply = %q", got)
	}
}

func TestFormatShortDur(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:                "0s",
		0:                           "0s",
		45 * time.Second:            "45s",
		5 * time.Minute:             "5m",
		2*time.Hour + 3*time.Minute: "2h3m",
	}
	for d, want := range tests {
		if got := formatShortDur(d); got != want {
			t.Errorf("formatShortDur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestQuotaAndCooldownEdges(t *testing.T) {
	rt := &Runtime{
		ListTokenQuota:  func() []TokenQuotaEntry { return []TokenQuotaEntry{{RatePerMin: 30, HitsInWindow: 3}} },
		ResetTokenQuota: func(string) int { return 0 },
		ListCooldowns:   func() []CooldownEntry { return []CooldownEntry{{Provider: "openai", Model: "gpt"}} },
	}
	if got := reply(t, rt, "/quota"); !strings.Contains(got, "(unnamed) — 30/min — 3/30 this minute") {
		t.Errorf("quota = %q", got)
	}
	if got := reply(t, rt, "/quota reset"); got != "No active token blocks." {
		t.Errorf("quota reset = %q", got)
	}
	if got := reply(t, rt, "/cooldowns list"); !strings.Contains(got, "openai/gpt — unknown") {
		t.Errorf("cooldowns = %q", got)
	}
}

func TestExecutorAndRegistryEdges(t *testing.T) {
	var nilEx *Executor
	if res := nilEx.Execute(context.Background(), Request{Text: "/help"}); res.Outcome != OutcomePassthrough || res.Command != "help" {
		t.Errorf("nil executor = %+v", res)
	}
	if res := NewExecutor(nil, nil).Execute(context.Background(), Request{Text: "/help"}); res.Outcome != OutcomePassthrough {
		t.Errorf("nil registry = %+v", res)
	}
	// A nil Reply is replaced, so a handler can always reply.
	if res := NewExecutor(NewRegistry(BuiltinDefinitions()), nil).Execute(context.Background(), Request{Text: "/start"}); res.Outcome != OutcomeHandled || res.Err != nil {
		t.Errorf("nil Reply = %+v", res)
	}

	reg := NewRegistry([]Definition{{Name: "a", Aliases: []string{"", "a", "b"}}, {Name: "b"}})
	if _, ok := reg.Lookup(""); ok {
		t.Error("empty name found")
	}
	if def, ok := reg.Lookup("b"); !ok || def.Name != "a" {
		t.Errorf("first registration wins: got %+v, %v", def, ok)
	}

	for _, in := range []string{"", "   ", "hello", "/", "/@bot"} {
		if name, ok := ParseCommandName(in); ok {
			t.Errorf("ParseCommandName(%q) = %q, true", in, name)
		}
	}
}
