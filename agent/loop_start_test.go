// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
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
	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// toolsOnAtAssembly is a context manager whose first assembly, after the
// message was stored and before the model loop starts, turns /tools on.
type toolsOnAtAssembly struct {
	capturingContextManager
	turnOn func()
	once   sync.Once
}

func (c *toolsOnAtAssembly) Assemble(ctx context.Context, req ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	c.once.Do(c.turnOn)
	return c.capturingContextManager.Assemble(ctx, req)
}

// /tools is read when the model loop starts: a change made after the turn
// was built, its message stored and its first request assembled is the one
// the turn's breadcrumbs follow.
func TestLoopStart_ToolsReadAfterFirstAssembly(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al, msgBus := tl.al, tl.msgBus
	tl.cfg.Agents.Defaults.StreamToolActivity = false
	agent := al.registry.Default()
	agent.Tools.Register(&mockDisplayTool{forUser: "unused"})
	if agent.Config != nil {
		agent.Config.Tools = []string{"*"}
	}
	setTestProvider(agent, &sequenceProvider{
		responses: []*spawnllm.LLMResponse{
			{ToolCalls: []spawnllm.ToolCall{{
				ID: "tc-1", Type: "function", Name: "display_tool",
				Function: &spawnllm.FunctionCall{Name: "display_tool", Arguments: `{}`},
			}}},
			{Content: "done"},
		},
		errors: []error{nil, nil},
	})
	opts := processOptions{SessionKey: "tools-late", Channel: "test", ChatID: "alice", UserMessage: "go"}
	al.setShowToolActivity(agent, opts.SessionKey, false)
	cm := &toolsOnAtAssembly{turnOn: func() { al.setShowToolActivity(agent, opts.SessionKey, true) }}
	entry := &cmEntry{cm: cm, sessionKey: opts.SessionKey}
	entry.touch()
	al.contextManagers.Store(agent.ID+":"+opts.SessionKey, entry)

	collected := make(chan bus.OutboundMessage, 16)
	subCtx := t.Context()
	go func() {
		for {
			msg, ok := msgBus.SubscribeOutbound(subCtx)
			if !ok {
				return
			}
			collected <- msg
		}
	}()
	if _, err := al.runAgentLoop(context.Background(), agent, opts); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	var breadcrumbs int
	for _, m := range untilMarker(t, msgBus, collected) {
		if strings.HasPrefix(m.Content, "🔧 ") {
			breadcrumbs++
		}
	}
	if breadcrumbs != 1 {
		t.Fatalf("breadcrumbs = %d, want 1: /tools was not read at loop start", breadcrumbs)
	}
}

// streamChannel is a channel that streams partial text and edits its
// placeholder, recording both.
type streamChannel struct {
	fakeChannel
	mu     sync.Mutex
	deltas []string
	edits  []string
}

func (c *streamChannel) StreamDelta(_ context.Context, _, _, delta string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deltas = append(c.deltas, delta)
	return nil
}

func (c *streamChannel) EditMessage(_ context.Context, _, _, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, content)
	return nil
}

func (c *streamChannel) recorded() (deltas, edits []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.deltas), slices.Clone(c.edits)
}

var _ channels.StreamCapable = (*streamChannel)(nil)

// A provider request asks for streaming only when the turn streams: on a
// channel that streams partial text, not on one that does not.
func TestLoopStart_ProviderRequestStreamsOnlyForAStreamingChannel(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	var mu sync.Mutex
	var streamFlags []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		streamFlags = append(streamFlags, body["stream"])
		mu.Unlock()
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi Alice"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`[DONE]`,
			} {
				if _, err := w.Write([]byte("data: " + chunk + "\n\n")); err != nil {
					t.Errorf("write: %v", err)
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hi Alice"},"finish_reason":"stop"}]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(t)
	cfg.Providers = append(cfg.Providers, config.Provider{Name: "wire", Protocol: "openai-chat", BaseURL: srv.URL + "/v1", APIKey: "k"})
	cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: "wire-model", Model: "alice-wire", Provider: "wire", Enabled: true})
	cfg.Agents.Defaults.Models = []string{"wire-model"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, realClients{})
	chManager, err := channels.NewManager(&config.Config{}, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	streaming := &streamChannel{}
	chManager.RegisterChannel("streaming", streaming)
	chManager.RegisterChannel("plain", &fakeChannel{})
	al.SetChannelManager(chManager)
	agent := al.registry.Default()

	for _, channel := range []string{"streaming", "plain"} {
		opts := processOptions{SessionKey: "s-" + channel, Channel: channel, ChatID: "alice", UserMessage: "hello"}
		if reply, err := al.runAgentLoop(context.Background(), agent, opts); err != nil || reply != "Hi Alice" {
			t.Fatalf("%s: runAgentLoop = %q, %v", channel, reply, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(streamFlags, []any{true, nil}) {
		t.Fatalf("stream flags sent = %v, want [true <nil>] (streaming channel, then plain)", streamFlags)
	}
	if deltas, _ := streaming.recorded(); strings.Join(deltas, "") != "Hi Alice" {
		t.Fatalf("streamed %q", deltas)
	}
}

// loopStartPanicStore is a session store whose per-session state read
// panics once armed: the /tools read at loop start fails.
type loopStartPanicStore struct {
	session.SessionStore
	armed   atomic.Bool
	mu      sync.Mutex
	pending []string // "set", "cleared"
}

func (s *loopStartPanicStore) GetCompactionState(string) (memory.CompactionState, error) {
	if s.armed.Load() {
		panic("Bob's loop-start bug")
	}
	return memory.CompactionState{}, nil
}

func (s *loopStartPanicStore) SetCompactionState(string, memory.CompactionState) error { return nil }

func (s *loopStartPanicStore) SetPendingTurn(key string) error {
	s.mu.Lock()
	s.pending = append(s.pending, "set")
	s.mu.Unlock()
	return s.SessionStore.SetPendingTurn(key)
}

func (s *loopStartPanicStore) ClearPendingTurn(key string) error {
	s.mu.Lock()
	s.pending = append(s.pending, "cleared")
	s.mu.Unlock()
	return s.SessionStore.ClearPendingTurn(key)
}

func (s *loopStartPanicStore) marker() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pending)
}

// armAtAssembly is a context manager whose first assembly runs arm.
type armAtAssembly struct {
	capturingContextManager
	arm func()
}

func (c *armAtAssembly) Assemble(ctx context.Context, req ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	if c.arm != nil {
		c.arm()
	}
	return c.capturingContextManager.Assemble(ctx, req)
}

// A panic in the set-up of a turn's model loop ends the turn as it always
// has: the sender is told "internal error while processing the message", the
// turn finishes once with an error, the pending marker and the pending
// source are cleared, and no progress heartbeat is left running.
func TestLoopStart_PanicEndsTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inHost: the host's own loop-start set-up panics (its /tools read,
		// before the heartbeat would start); otherwise a BeforeLoop after it
		// does, once the heartbeat has edited the placeholder.
		inHost bool
		want   string
	}{
		{name: "host set-up", inHost: true, want: "Bob's loop-start bug"},
		{name: "after the heartbeat started", want: "Bob's later bug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
			tl := newTestAgentLoop(t)
			al := tl.al
			tl.cfg.Agents.Defaults.ProgressInterval = 1
			agent := al.registry.Default()
			setTestProvider(agent, &finalLLMProvider{})
			store := &loopStartPanicStore{SessionStore: agent.Sessions}
			agent.Sessions = store
			t.Cleanup(func() { agent.Sessions = store.SessionStore })
			chManager, err := channels.NewManager(&config.Config{}, bus.NewMessageBus(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ch := &streamChannel{}
			chManager.RegisterChannel("telegram", ch)
			chManager.RecordPlaceholder("telegram", "alice", "placeholder-1")
			al.SetChannelManager(chManager)

			opts := processOptions{SessionKey: "agent:main:telegram:direct:alice", Channel: "telegram", ChatID: "alice", UserMessage: "hello"}
			cm := &armAtAssembly{}
			if tc.inHost {
				cm.arm = func() { store.armed.Store(true) }
			}
			var finished []hook.TurnFinished
			plugins := []hook.Plugin{{Name: "tap", OnEvent: func(_ context.Context, ev hook.Event) {
				if f, ok := ev.(hook.TurnFinished); ok {
					finished = append(finished, f)
				}
			}}}
			if !tc.inHost {
				plugins = append(plugins, hook.Plugin{Name: "later", BeforeLoop: func(context.Context, *hook.Loop) error {
					// The heartbeat runs: it edits the placeholder.
					for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
						if _, edits := ch.recorded(); len(edits) > 0 {
							panic("Bob's later bug")
						}
					}
					t.Error("the heartbeat never edited the placeholder")
					panic("Bob's later bug")
				}})
			}

			var userErr error
			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatal("the turn did not panic")
					}
					userErr = messagePanicked(bus.InboundMessage{Channel: opts.Channel, ChatID: opts.ChatID}, r)
				}()
				tm := al.selectTurnModel(agent, opts.SessionKey)
				_, _, _, oerr := runTestTurn(t, context.Background(), al, agent, opts, tm, cm, plugins, nil)
				t.Fatalf("turnOutcome returned %v instead of panicking", oerr)
			}()
			if userErr == nil || userErr.Error() != "internal error while processing the message: "+tc.want {
				t.Fatalf("sender told %v", userErr)
			}
			if len(finished) != 1 || finished[0].Outcome != hook.OutcomeError {
				t.Fatalf("TurnFinished = %+v, want one with an error", finished)
			}
			if perr, ok := errors.AsType[*hook.PanicError](finished[0].Err); !ok || perr.Callback != "BeforeLoop" {
				t.Fatalf("TurnFinished error = %v, want BeforeLoop's panic", finished[0].Err)
			}
			if got := store.marker(); !slices.Equal(got, []string{"set", "cleared"}) {
				t.Fatalf("pending marker = %v, want set then cleared", got)
			}
			if _, recorded := testStateManager(t, al, agent.ID).GetPendingTurn(opts.SessionKey); recorded {
				t.Fatal("the pending source was left recorded")
			}
			_, before := ch.recorded()
			time.Sleep(1500 * time.Millisecond)
			if _, edits := ch.recorded(); len(edits) != len(before) {
				t.Fatalf("the heartbeat kept editing the placeholder after the turn: %q", edits[len(before):])
			}
		})
	}
}
