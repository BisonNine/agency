package channels

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/health"
)

// noSend is a mockChannel send function that fails the test.
func noSend(t *testing.T) func(context.Context, bus.OutboundMessage) error {
	t.Helper()
	return func(context.Context, bus.OutboundMessage) error {
		t.Error("the channel was sent a message")
		return nil
	}
}

func failingEditor(t *testing.T) *mockMessageEditor {
	t.Helper()
	e := &mockMessageEditor{editFn: func(context.Context, string, string, string) error {
		return errors.New("message too old to edit")
	}}
	e.sendFn = noSend(t)
	return e
}

func TestUpdatePlaceholder_NotUpdated(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *Manager)
	}{
		{"empty placeholder id", func(m *Manager) {
			m.RegisterChannel("test", failingEditor(t))
			m.RecordPlaceholder("test", "c", "")
		}},
		{"channel gone", func(m *Manager) {
			m.RecordPlaceholder("test", "c", "ph")
		}},
		{"channel cannot edit", func(m *Manager) {
			m.RegisterChannel("test", newLifecycleFake("test", nil))
			m.RecordPlaceholder("test", "c", "ph")
		}},
		{"edit fails", func(m *Manager) {
			m.RegisterChannel("test", failingEditor(t))
			m.RecordPlaceholder("test", "c", "ph")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager()
			tt.setup(m)
			if m.UpdatePlaceholder(context.Background(), "test", "c", "still working") {
				t.Fatal("UpdatePlaceholder = true, want false")
			}
			if _, ok := m.placeholders.Load("test:c"); !ok {
				t.Fatal("UpdatePlaceholder consumed the placeholder")
			}
		})
	}
}

func TestSendPlaceholder_NotSent(t *testing.T) {
	failing, _ := newBusyChannel(nil)
	failing.failPlaceholder = true
	empty, _ := newBusyChannel(nil)
	empty.placeholderID = ""
	tests := []struct {
		name string
		ch   Channel // nil: no channel registered
	}{
		{"channel gone", nil},
		{"channel cannot send placeholders", newLifecycleFake("test", nil)},
		{"send fails", failing},
		{"no placeholder id", empty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager()
			if tt.ch != nil {
				m.RegisterChannel("test", tt.ch)
			}
			if m.SendPlaceholder(context.Background(), "test", "c") {
				t.Fatal("SendPlaceholder = true, want false")
			}
			if _, ok := m.placeholders.Load("test:c"); ok {
				t.Fatal("a placeholder was recorded")
			}
		})
	}
}

// DismissInbound with nothing usable to clear does nothing, and a placeholder
// it can neither delete nor edit is still forgotten.
func TestDismissInbound_NothingToClear(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *Manager)
	}{
		{"no placeholder", func(m *Manager) { m.RegisterChannel("test", failingEditor(t)) }},
		{"empty placeholder id", func(m *Manager) {
			m.RegisterChannel("test", failingEditor(t))
			m.RecordPlaceholder("test", "c", "")
		}},
		{"channel gone", func(m *Manager) { m.RecordPlaceholder("test", "c", "ph") }},
		{"edit fails", func(m *Manager) {
			m.RegisterChannel("test", failingEditor(t))
			m.RecordPlaceholder("test", "c", "ph")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager()
			tt.setup(m)
			m.DismissInbound(context.Background(), "test", "c", "m1")
			if _, ok := m.placeholders.Load("test:c"); ok {
				t.Fatal("placeholder still recorded after DismissInbound")
			}
		})
	}
}

type failingStream struct {
	mockChannel
	calls int
}

func (s *failingStream) StreamDelta(context.Context, string, string, string) error {
	s.calls++
	return errors.New("stream closed")
}

// A failing stream delta is swallowed: streaming is best effort.
func TestStreamDelta_ErrorSwallowed(t *testing.T) {
	m := newTestManager()
	sc := &failingStream{}
	m.channels["stream"] = sc
	m.StreamDelta(context.Background(), "stream", "c", "", "partial")
	if sc.calls != 1 {
		t.Fatalf("StreamDelta calls = %d, want 1", sc.calls)
	}
}

func TestNewManager_BuildHookSeesEveryChannel(t *testing.T) {
	ff := registerFakeFactories(t)
	hooked := map[string]Channel{}
	m, err := NewManager(discordAndSlack(), bus.NewMessageBus(), nil, WithBuildHook(func(name string, ch Channel) {
		hooked[name] = ch
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(hooked) != 2 || hooked["discord"] != ff.last("discord") || hooked["slack"] != ff.last("slack") {
		t.Fatalf("hook saw %v, want discord and slack as built", hooked)
	}
	if ch, ok := m.Channel("discord"); !ok || ch != hooked["discord"] {
		t.Fatal("Channel does not return the built channel")
	}
	if _, ok := m.Channel("matrix"); ok {
		t.Fatal("Channel found a channel that is not configured")
	}
}

// An empty allow_from only warns: the channel is still built.
func TestNewManager_EmptyAllowFromStillBuilt(t *testing.T) {
	registerFakeFactories(t)
	cfg := &config.Config{}
	cfg.Channels.Discord = config.DiscordConfig{Enabled: true, Token: "d1"}
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.GetChannel("discord"); !ok {
		t.Fatal("channel with an empty allow_from was not built")
	}
}

// Without a discovery function, a daemon with no pinned accounts gets one
// auto-selecting channel carrying the daemon's defaults.
func TestResolveSecMsgAccounts_NoDiscoveryRegistered(t *testing.T) {
	withDiscovery(t, nil)
	m := &Manager{}
	got, ok := m.resolveSecMsgAccounts(config.SecMsgConfig{
		Name:      "signal",
		Address:   "127.0.0.1:9600",
		AllowFrom: config.FlexibleStringSlice{"+15551112222"},
	})
	if !ok || len(got) != 1 || got[0].Account != "" {
		t.Fatalf("accounts = %+v ok=%v, want one auto-selecting account", got, ok)
	}
	if len(got[0].AllowFrom) != 1 || got[0].AllowFrom[0] != "+15551112222" {
		t.Fatalf("account did not inherit the daemon allow_from: %+v", got[0].AllowFrom)
	}
}

// webhookChannel serves a webhook and a health endpoint.
type webhookChannel struct{ *lifecycleFake }

func (webhookChannel) WebhookPath() string { return "/hook/" }
func (webhookChannel) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusAccepted)
}
func (webhookChannel) HealthPath() string { return "/hook-health" }
func (webhookChannel) HealthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot)
}

func TestRegisterHandlers(t *testing.T) {
	m := newTestManager()
	m.RegisterChannel("hooked", webhookChannel{newLifecycleFake("hooked", nil)})
	m.RegisterChannel("plain", newLifecycleFake("plain", nil))
	mux := http.NewServeMux()
	m.RegisterHandlers(mux, health.NewServer("127.0.0.1", 0))

	for path, want := range map[string]int{
		"/hook/event":  http.StatusAccepted,
		"/hook-health": http.StatusTeapot,
		"/health":      http.StatusOK,
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rr.Code, want)
		}
	}
}

func TestRegisterHandlers_NoHealthServer(t *testing.T) {
	m := newTestManager()
	m.RegisterChannel("hooked", webhookChannel{newLifecycleFake("hooked", nil)})
	mux := http.NewServeMux()
	m.RegisterHandlers(mux, nil)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /health = %d, want 404 without a health server", rr.Code)
	}
}

// A backlog left when the worker's context has ended is not sent: each
// message is reported with the context's error.
func TestRunWorker_BacklogAfterCancel(t *testing.T) {
	m := newTestManager()
	ch := &mockChannel{sendFn: noSend(t)}
	w := newChannelWorker("test", ch)
	reports, report := deliveryReport()
	w.backlog = []bus.OutboundMessage{{Channel: "test", ChatID: "c", Content: "late", OnDelivery: report}}
	w.closeQueues()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.runWorker(ctx, "test", w)
	if err := expectDelivery(t, reports); !errors.Is(err, context.Canceled) {
		t.Fatalf("backlog reported %v, want context.Canceled", err)
	}
}

// A send or media send whose context has ended before the rate limiter lets
// it through is not attempted and raises no alert.
func TestSendWithRetry_CancelledBeforeRateLimit(t *testing.T) {
	m := newTestManager()
	rec := &alertRecorder{}
	m.SetAlerter(rec)
	ch := &mediaMock{}
	ch.sendFn = noSend(t)
	w := newChannelWorker("test", ch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.sendWithRetry(ctx, "test", w, bus.OutboundMessage{Channel: "test", ChatID: "c"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sendWithRetry = %v, want context.Canceled", err)
	}
	if err := m.sendMediaWithRetry(ctx, "test", w, bus.OutboundMediaMessage{Channel: "test", ChatID: "c"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sendMediaWithRetry = %v, want context.Canceled", err)
	}
	if ch.calls != 0 {
		t.Fatalf("SendMedia called %d times", ch.calls)
	}
	if n := recorded(rec); n != 0 {
		t.Fatalf("%d alerts for a cancelled send", n)
	}
}

// The dispatcher stops once routing finds its context ended, reporting the
// message it had in hand.
func TestDispatchLoop_StopsWhenRoutingSeesCancel(t *testing.T) {
	m := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscribed := 0
	subscribe := func(context.Context) (bus.OutboundMessage, bool) {
		subscribed++
		if subscribed > 1 {
			t.Error("the dispatcher subscribed again after its context ended")
			return bus.OutboundMessage{}, false
		}
		cancel() // shutdown arrives with the message
		return bus.OutboundMessage{Channel: "test", ChatID: "c"}, true
	}
	var dropped []error
	dispatchLoop(ctx, m, subscribe,
		func(msg bus.OutboundMessage) string { return msg.Channel },
		func(w *channelWorker) chan bus.OutboundMessage { return w.queue },
		func(w *channelWorker) <-chan struct{} { return w.done },
		holdText, func(_ bus.OutboundMessage, err error) { dropped = append(dropped, err) },
		"start", "stop", "unknown", "no worker")
	if len(dropped) != 1 || !errors.Is(dropped[0], context.Canceled) {
		t.Fatalf("dropped %v, want one context.Canceled", dropped)
	}
}

// A message waiting for room on a full queue is dropped with the context's
// error once the context ends, and routing reports that it ended.
func TestRouteOutbound_CancelledWhileQueueFull(t *testing.T) {
	m := newTestManager()
	m.channels["test"] = &mockChannel{}
	w := &channelWorker{queue: make(chan bus.OutboundMessage, 1), done: make(chan struct{})}
	w.queue <- bus.OutboundMessage{Content: "ahead"} // no reader: the queue stays full
	m.workers["test"] = w
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dropped error
	ok := routeOutbound(ctx, m, "test", bus.OutboundMessage{Channel: "test"},
		func(w *channelWorker) chan bus.OutboundMessage { return w.queue },
		func(w *channelWorker) <-chan struct{} {
			cancel() // shutdown while the enqueue waits
			return w.done
		},
		holdText, func(_ bus.OutboundMessage, err error) { dropped = err }, "unknown", "no worker")
	if ok {
		t.Fatal("routeOutbound = true after its context ended")
	}
	if !errors.Is(dropped, context.Canceled) {
		t.Fatalf("dropped with %v, want context.Canceled", dropped)
	}
}

func TestCloseQueues_Twice(t *testing.T) {
	w := newChannelWorker("test", &mockChannel{})
	w.closeQueues()
	w.closeQueues() // must not close the queues again
	if !w.closed {
		t.Fatal("worker not marked closed")
	}
}

// Media published on the bus reaches the channel's SendMedia through the
// media dispatcher and worker, and its sender hears it was delivered.
func TestDispatchOutboundMedia_DeliversThroughWorker(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	m := newTestManager()
	m.bus = mb
	ch := &mediaMock{}
	ch.sendFn = noSend(t)
	w := newChannelWorker("files", ch)
	m.channels["files"] = ch
	m.workers["files"] = w
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runMediaWorker(ctx, "files", w)
	go m.dispatchOutboundMedia(ctx)

	delivered := make(chan error, 1)
	if err := mb.PublishOutboundMedia(ctx, bus.OutboundMediaMessage{
		Channel: "files", ChatID: "c", OnDelivery: func(err error) { delivered <- err },
	}); err != nil {
		t.Fatal(err)
	}
	if err := awaitErr(t, "media delivery", func() error { return <-delivered }); err != nil {
		t.Fatalf("OnDelivery = %v, want nil", err)
	}
	if ch.calls != 1 {
		t.Fatalf("SendMedia calls = %d, want 1", ch.calls)
	}
}

func TestGetStatus(t *testing.T) {
	m := newTestManager()
	up := newLifecycleFake("up", nil)
	up.SetRunning(true)
	m.RegisterChannel("up", up)
	m.RegisterChannel("down", newLifecycleFake("down", nil))

	got := m.GetStatus()
	want := map[string]map[string]any{
		"up":   {"enabled": true, "running": true},
		"down": {"enabled": true, "running": false},
	}
	if len(got) != len(want) {
		t.Fatalf("GetStatus = %v, want %v", got, want)
	}
	for name, w := range want {
		s, ok := got[name].(map[string]any)
		if !ok || s["enabled"] != w["enabled"] || s["running"] != w["running"] {
			t.Errorf("status[%s] = %v, want %v", name, got[name], w)
		}
	}
}

// UnregisterChannel removes a running channel: it gets nothing more, its
// worker finishes, and messages for it are reported as unknown.
func TestUnregisterChannel_Running(t *testing.T) {
	registerFakeFactories(t)
	m, _ := startedManager(t, discordAndSlack(), nil)
	m.mu.RLock()
	w := m.workers["discord"]
	m.mu.RUnlock()

	m.UnregisterChannel("discord")
	select {
	case <-w.done:
	default:
		t.Fatal("the removed channel's worker is still running")
	}
	if _, ok := m.GetChannel("discord"); ok {
		t.Fatal("channel still registered")
	}
	reports, report := deliveryReport()
	routeText(m, bus.OutboundMessage{Channel: "discord", ChatID: "c", OnDelivery: report})
	if err := expectDelivery(t, reports); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("message to the removed channel reported %v, want ErrUnknownChannel", err)
	}
	if _, ok := m.GetChannel("slack"); !ok {
		t.Fatal("the other channel was removed too")
	}
}

// UnregisterChannel of a channel still starting cancels its start and drops
// what was held for it as not running.
func TestUnregisterChannel_WhileStarting(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	m := newTestManager()
	m.bus = mb
	slow := newLifecycleFake("slow", mb)
	slow.gate = make(chan struct{}) // opened only by the cancellation
	m.RegisterChannel("slow", slow)
	started := startGated(t, m, slow)
	t.Cleanup(func() {
		if err := m.StopAll(context.Background()); err != nil {
			t.Error(err)
		}
	})

	reports, report := deliveryReport()
	routeText(m, bus.OutboundMessage{Channel: "slow", ChatID: "c", OnDelivery: report})
	m.UnregisterChannel("slow")
	if err := expectDelivery(t, reports); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("held message reported %v, want ErrNotRunning", err)
	}
	await(t, started, "StartAll")
	m.mu.RLock()
	w := m.workers["slow"]
	m.mu.RUnlock()
	if w != nil {
		t.Fatal("worker registered for a channel removed while starting")
	}
}

func TestSendToChannel(t *testing.T) {
	t.Run("unknown channel", func(t *testing.T) {
		m := newTestManager()
		if err := m.SendToChannel(context.Background(), "nope", "c", "hi"); !errors.Is(err, ErrUnknownChannel) {
			t.Fatalf("SendToChannel = %v, want ErrUnknownChannel", err)
		}
	})
	t.Run("queued on the worker", func(t *testing.T) {
		m := newTestManager()
		ch := &mockChannel{sendFn: noSend(t)}
		w := newChannelWorker("test", ch)
		m.channels["test"], m.workers["test"] = ch, w
		if err := m.SendToChannel(context.Background(), "test", "c", "hi"); err != nil {
			t.Fatal(err)
		}
		select {
		case msg := <-w.queue:
			if msg.Channel != "test" || msg.ChatID != "c" || msg.Content != "hi" {
				t.Fatalf("queued %+v", msg)
			}
		default:
			t.Fatal("nothing queued")
		}
	})
	t.Run("cancelled while the queue is full", func(t *testing.T) {
		m := newTestManager()
		ch := &mockChannel{sendFn: noSend(t)}
		w := &channelWorker{queue: make(chan bus.OutboundMessage), done: make(chan struct{})}
		m.channels["test"], m.workers["test"] = ch, w
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := m.SendToChannel(ctx, "test", "c", "hi"); !errors.Is(err, context.Canceled) {
			t.Fatalf("SendToChannel = %v, want context.Canceled", err)
		}
	})
	for _, tc := range []struct {
		name   string
		worker *channelWorker
	}{
		{"sent directly without a worker", nil},
		{"sent directly when the worker is closed", exitedWorker(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager()
			sendErr := errors.New("rejected")
			ch := &mockChannel{sendFn: func(context.Context, bus.OutboundMessage) error { return sendErr }}
			m.channels["test"] = ch
			if tc.worker != nil {
				m.workers["test"] = tc.worker
			}
			if err := m.SendToChannel(context.Background(), "test", "c", "hi"); !errors.Is(err, sendErr) {
				t.Fatalf("SendToChannel = %v, want the channel's own error", err)
			}
			if len(ch.sentMessages) != 1 || ch.sentMessages[0].Content != "hi" {
				t.Fatalf("sent %+v, want the message once", ch.sentMessages)
			}
		})
	}
}

// A worker whose context has ended reports what is still queued as not
// sent, whether it takes the message from the queue or sees the end first.
func TestRunWorker_QueuedAtShutdownReported(t *testing.T) {
	m := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		ch := &mediaMock{}
		ch.sendFn = noSend(t)
		w := newChannelWorker("test", ch)
		var textErr, mediaErr error
		w.queue <- bus.OutboundMessage{Channel: "test", OnDelivery: func(err error) { textErr = err }}
		w.mediaQueue <- bus.OutboundMediaMessage{Channel: "test", OnDelivery: func(err error) { mediaErr = err }}
		w.closeQueues()
		m.runWorker(ctx, "test", w)
		m.runMediaWorker(ctx, "test", w)
		if !errors.Is(textErr, context.Canceled) || !errors.Is(mediaErr, context.Canceled) {
			t.Fatalf("queued text reported %v, media %v; want context.Canceled", textErr, mediaErr)
		}
		if ch.calls != 0 {
			t.Fatal("media sent after shutdown")
		}
	}
}
