package channels

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
)

// stopFailingFake is a lifecycleFake whose Stop fails (after recording it).
type stopFailingFake struct{ *lifecycleFake }

var errStopFailed = errors.New("stop timed out")

func (f stopFailingFake) Stop(ctx context.Context) error {
	if err := f.lifecycleFake.Stop(ctx); err != nil {
		return err
	}
	return errStopFailed
}

func TestStartAll_NoChannels(t *testing.T) {
	m := newTestManager()
	if err := m.StartAll(context.Background()); err == nil || err.Error() != "no channels enabled" {
		t.Fatalf("StartAll = %v, want \"no channels enabled\"", err)
	}
	if m.dispatchCtx != nil {
		t.Fatal("dispatchers started with no channels")
	}
}

// A channel replaced while its first Start runs gets no worker when that
// start finishes, and what was held for it is dropped as not running.
func TestStartAll_ChannelReplacedWhileStarting(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	m := newTestManager()
	m.bus = mb
	slow := newLifecycleFake("slow", mb)
	slow.gate = make(chan struct{})
	m.RegisterChannel("slow", slow)
	started := startGated(t, m, slow)
	t.Cleanup(func() {
		if err := m.StopAll(context.Background()); err != nil {
			t.Error(err)
		}
	})

	reports, report := deliveryReport()
	routeText(m, bus.OutboundMessage{Channel: "slow", ChatID: "c", OnDelivery: report})
	m.RegisterChannel("slow", newLifecycleFake("slow", mb))
	close(slow.gate)
	await(t, started, "StartAll")

	if err := expectDelivery(t, reports); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("held message reported %v, want ErrNotRunning", err)
	}
	m.mu.RLock()
	w, cancel := m.workers["slow"], m.cancels["slow"]
	m.mu.RUnlock()
	if w != nil || cancel != nil {
		t.Fatalf("worker %v cancel %v registered for a replaced channel", w != nil, cancel != nil)
	}
}

// Cancelled while waiting to retry, the retry loop ends without starting the
// channel again.
func TestRetryChannelStart_CancelledWhileWaiting(t *testing.T) {
	m := newTestManager()
	m.clock = newRecordingClock(false) // the wait never ends by itself
	ch := &flakyStartChannel{}
	m.RegisterChannel("dead", ch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.retryChannelStart(ctx, "dead", ch)
	if n := ch.attempts.Load(); n != 0 {
		t.Fatalf("Start attempts = %d, want 0", n)
	}
}

// cancellingStart fails its Start because the context it runs on ended
// meanwhile (a shutdown).
type cancellingStart struct {
	*lifecycleFake
	cancel context.CancelFunc
}

func (c cancellingStart) Start(context.Context) error {
	c.starts.Add(1)
	c.cancel()
	return errors.New("connection closed")
}

// A retry that fails because the channel is shutting down ends the retries
// without an alert.
func TestRetryChannelStart_FailsDuringShutdown(t *testing.T) {
	m := newTestManager()
	rec := &alertRecorder{}
	m.SetAlerter(rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := cancellingStart{newLifecycleFake("closing", nil), cancel}
	m.RegisterChannel("closing", ch)

	m.retryChannelStart(ctx, "closing", ch)
	if n := ch.starts.Load(); n != 1 {
		t.Fatalf("Start attempts = %d, want 1", n)
	}
	if n := recorded(rec); n != 0 || len(waitsOf(m)) != 1 {
		t.Fatalf("%d alerts, %d waits; want none and one", n, len(waitsOf(m)))
	}
}

// A retry that succeeds for a channel no longer registered stops it again,
// even when that stop fails, and registers no worker.
func TestRetryChannelStart_LateSuccessStopFails(t *testing.T) {
	m := newTestManager()
	ch := stopFailingFake{newLifecycleFake("gone", nil)}

	m.retryChannelStart(t.Context(), "gone", ch)
	if n := ch.starts.Load(); n != 1 {
		t.Fatalf("Start attempts = %d, want 1", n)
	}
	if n := ch.stops.Load(); n != 1 {
		t.Fatalf("Stop calls = %d, want 1", n)
	}
	if _, ok := m.workers["gone"]; ok {
		t.Fatal("worker registered for a channel that is not registered")
	}
}

// Once its context ends, waitWorker cancels the worker and returns when its
// goroutines have exited.
func TestWaitWorker_CancelsAfterDeadline(t *testing.T) {
	w := &channelWorker{done: make(chan struct{}), mediaDone: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := false
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		waitWorker(ctx, "test", w, func() {
			cancelled = true
			close(w.done)
			close(w.mediaDone)
		})
	}()
	await(t, finished, "waitWorker")
	if !cancelled {
		t.Fatal("waitWorker did not cancel the worker")
	}
}

// A channel whose Stop fails is still replaced by a reload (here a built-in,
// which every reload rebuilds), and the new instance gets the worker.
func TestReload_RebuildsChannelWhoseStopFails(t *testing.T) {
	var built []stopFailingFake
	registerTestBuiltin(t, "teststopfail", func(_ *config.Config, b *bus.MessageBus) (Channel, error) {
		ch := stopFailingFake{newLifecycleFake("teststopfail", b)}
		built = append(built, ch)
		return ch, nil
	})
	mb := bus.NewMessageBus()
	defer mb.Close()
	m, err := NewManager(&config.Config{}, mb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.StopAll(context.Background()); !errors.Is(err, errStopFailed) {
			t.Errorf("StopAll = %v, want the channel's stop error", err)
		}
	})

	res := m.Reload(context.Background(), &config.Config{}, nil)
	if !slices.Equal(res.Restarted, []string{"teststopfail"}) {
		t.Fatalf("reload = %+v, want teststopfail restarted", res)
	}
	if len(built) != 2 || built[0].stops.Load() != 1 {
		t.Fatalf("built %d, old instance stopped %d times; want 2 and 1", len(built), built[0].stops.Load())
	}
	m.mu.RLock()
	w := m.workers["teststopfail"]
	m.mu.RUnlock()
	if w == nil || w.ch != built[1] {
		t.Fatal("the worker is not the rebuilt channel's")
	}
}
