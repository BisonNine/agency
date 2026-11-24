// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
)

// hooksEdgeLoop is a loop with the default test configuration and its bus.
func hooksEdgeLoop(t *testing.T, change ...func(*config.Config)) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := newTestConfig(t)
	for _, c := range change {
		c(cfg)
	}
	msgBus := bus.NewMessageBus()
	return mustNewAgentLoop(t, cfg, msgBus, &mockProvider{}, nil), msgBus
}

// hooksEdgePlugin returns the plugin named name.
func hooksEdgePlugin(t *testing.T, ps []hook.Plugin, name string) hook.Plugin {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no %q plugin", name)
	return hook.Plugin{}
}

// The host's plugins do nothing for a context that carries no host turn.
func TestTurnHooks_PluginsWithoutHostTurn(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, msgBus := hooksEdgeLoop(t)
	ctx := context.Background()
	agent := al.GetRegistry().Default()

	al.compactionNoticePlugin().OnCompaction(ctx, hook.Compaction{Phase: hook.CompactionOverflow})

	ps := al.turnPlugins(agent)
	if res, err := hooksEdgePlugin(t, ps, "session").BeforeTurn(ctx, &hook.Turn{}); res != nil || err != nil {
		t.Fatalf("session BeforeTurn = %v, %v; want nothing", res, err)
	}
	if text, ok := hooksEdgePlugin(t, ps, "vision-describe").DescribeImages(ctx, hook.UnseenImages{Images: []string{"x"}}); text != "" || ok {
		t.Fatalf("DescribeImages = %q, %v; want nothing", text, ok)
	}
	al.logModelResponse(agent)(ctx, &hook.ModelInfo{}, &hook.ModelResponse{})

	agent.HumanModel = "Bob (human)"
	person := al.turnPlugins(agent)
	if len(person) != 1 || person[0].Name != "person" {
		t.Fatalf("a person's plugins = %v, want only person", person)
	}
	if res, err := person[0].BeforeTurn(ctx, &hook.Turn{}); res != nil || err != nil {
		t.Fatalf("person BeforeTurn = %v, %v; want nothing", res, err)
	}
	noOutbound(t, msgBus)
}

// A turn given no turn model selects the agent's own.
func TestTurnHooks_RunModelTurnSelectsModel(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()
	key := routing.BuildAgentMainSessionKey(agent.ID)
	cm, release := al.getContextManager(agent, key)
	defer release()
	reply, err := al.runModelTurn(context.Background(), agent, processOptions{
		SessionKey: key, Channel: "subagent", ChatID: key, UserMessage: "hi",
	}, cm, nil)
	if err != nil || reply != "Mock response" {
		t.Fatalf("runModelTurn = %q, %v; want the model's answer", reply, err)
	}
}

// A callback's or the engine's panic reads as the panic's value and unwraps
// to the SDK's error.
func TestTurnHooks_PanicErrors(t *testing.T) {
	cb := &hook.PanicError{Value: "callback boom"}
	var err error = &callbackPanicError{perr: cb}
	if err.Error() != "callback boom" {
		t.Fatalf("Error = %q", err.Error())
	}
	if got := new(*hook.PanicError); !errors.As(err, got) || *got != cb {
		t.Fatal("does not unwrap to the callback's panic")
	}
	eng := &loop.EnginePanicError{Value: "engine boom"}
	err = &enginePanicError{perr: eng}
	if err.Error() != "engine boom" {
		t.Fatalf("Error = %q", err.Error())
	}
	if got := new(*loop.EnginePanicError); !errors.As(err, got) || *got != eng {
		t.Fatal("does not unwrap to the engine's panic")
	}
}

// A callback's message is of kind callback; a sender id is its source when
// it has no name.
func TestTurnHooks_TurnMessage(t *testing.T) {
	msg := turnMessage(processOptions{UserMessage: "done", SenderID: "callback:42"})
	if msg.Kind != "callback" || msg.Source != "callback:42" || msg.Text != "done" {
		t.Fatalf("turnMessage = %+v", msg)
	}
}

// Only a model with file_edit gets the file_edit steering.
func TestTurnHooks_FileEditRepeatHint(t *testing.T) {
	if hint := fileEditRepeatHint([]string{"file_read_lines", "file_edit"}); !strings.Contains(hint, "file_edit") {
		t.Fatalf("hint = %q, want the file_edit steering", hint)
	}
	if hint := fileEditRepeatHint([]string{"file_read_lines"}); hint != "" {
		t.Fatalf("hint = %q, want none", hint)
	}
}

// A model the configuration does not have has vision off.
func TestTurnHooks_ModelFactsUnknownModel(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t)
	c := al.modelFacts(nil, "no-such-model")
	if c.Name != "no-such-model" || c.Limits.Vision != modelspec.VisionOff {
		t.Fatalf("modelFacts = %+v", c)
	}
}

// A disabled model's "disabled" reason is left unnamed in the notice.
func TestTurnHooks_FallbackAttemptsDisabled(t *testing.T) {
	got := fallbackAttempts([]hook.Attempt{
		{Model: hook.ModelRef{Name: "a"}, Disabled: true, Reason: "disabled"},
		{Model: hook.ModelRef{Name: "b"}, Reason: "rate_limit"},
	})
	if len(got) != 2 || got[0].Reason != "" || !got[0].Disabled || got[1].Reason != "rate_limit" {
		t.Fatalf("fallbackAttempts = %+v", got)
	}
}

// The overflow notice is not posted on an internal channel; a bus that
// refuses it is logged.
func TestTurnHooks_OverflowNotice(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksEdgeLoop(t)
	al.publishOverflowNotice(context.Background(), processOptions{Channel: "system", ChatID: "x"})
	noOutbound(t, msgBus)
	msgBus.Close()
	al.publishOverflowNotice(context.Background(), processOptions{Channel: "telegram", ChatID: "42"})
	if !strings.Contains(logs.String(), "Failed to publish compression notice") {
		t.Fatalf("the refused notice was not logged: %s", logs.String())
	}
}

// A refused eviction notice and a refused narration are logged.
func TestTurnHooks_NoticePublishFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	on := true
	al, msgBus := hooksEdgeLoop(t, func(cfg *config.Config) {
		cfg.Agents.Defaults.ContextEviction = &config.ContextEvictionConfig{NotifyUser: &on}
		cfg.Agents.Defaults.StreamToolActivity = true
	})
	agent := al.GetRegistry().Default()
	msgBus.Close()
	opts := processOptions{Channel: "telegram", ChatID: "42"}
	al.publishEvictionNotice(context.Background(), agent, opts, []ctxengine.EvictionEvent{{Tool: "file_read_bytes", Resource: "a.txt", Bytes: 10}})
	al.publishNarration(context.Background(), opts, "Let me check.")
	for _, want := range []string{"Failed to publish eviction notice", "Failed to publish inter-tool narration"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("%q was not logged: %s", want, logs.String())
		}
	}
}

// A finished call without a result records nothing.
func TestTurnHooks_RecordToolCallWithoutResult(t *testing.T) {
	const key = "hooks-cov-no-result"
	recordToolCall(context.Background(), nil, processOptions{SessionKey: key}, hook.ToolCallFinished{Call: hook.ToolCall{Name: "x"}})
	if calls, _, _ := tools.EndToolStats(key); calls != 0 {
		t.Fatalf("calls = %d, want none recorded", calls)
	}
}

// Without a vision side-model, tool images get no description.
func TestTurnHooks_DescribeToolImagesWithoutVision(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t)
	agent := al.GetRegistry().Default()
	agent.VisionClients = nil
	if text, ok := al.describeToolImages(context.Background(), agent, processOptions{}, "m", hook.UnseenImages{Images: []string{"x"}}); text != "" || ok {
		t.Fatalf("describeToolImages = %q, %v; want nothing", text, ok)
	}
}
