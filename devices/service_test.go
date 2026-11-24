// Agency
// License: MIT

package devices

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/devices/events"
)

// streamSource emits evs and then closes; Stop reports stopErr.
type streamSource struct {
	evs     []*events.DeviceEvent
	stopErr error
	stopped bool
}

func (s *streamSource) Kind() events.Kind { return events.KindUSB }
func (s *streamSource) Start(context.Context) (<-chan *events.DeviceEvent, error) {
	ch := make(chan *events.DeviceEvent, len(s.evs))
	for _, ev := range s.evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}
func (s *streamSource) Stop() error { s.stopped = true; return s.stopErr }

// Events from a started source are delivered to the target; nil events are
// skipped.
func TestService_DeliversSourceEvents(t *testing.T) {
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	s := NewService(Config{Enabled: true, Target: func() (string, string, bool) { return "telegram", "42", true }})
	s.SetBus(msgBus)
	src := &streamSource{evs: []*events.DeviceEvent{
		nil,
		{Action: events.ActionAdd, Kind: events.KindUSB, Vendor: "Acme", Product: "Stick"},
	}}
	s.sources = append(s.sources, src)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg, ok := msgBus.SubscribeOutbound(ctx)
	if !ok {
		t.Fatal("no notification delivered")
	}
	if msg.Channel != "telegram" || msg.ChatID != "42" || msg.Content != "🔌 Device Connected\n\nType: usb\nDevice: Acme Stick\n" {
		t.Errorf("notification = %+v", msg)
	}
}

func TestService_StartDisabledOrEmpty(t *testing.T) {
	for _, s := range []*Service{NewService(Config{}), NewService(Config{Enabled: true})} {
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.cancel != nil {
			t.Error("a service with nothing to run started anyway")
		}
		s.Stop()
	}
}

func TestNewService_MonitorUSB(t *testing.T) {
	if n := len(NewService(Config{Enabled: true, MonitorUSB: true}).sources); n != 1 {
		t.Errorf("enabled with USB: %d sources, want 1", n)
	}
	if n := len(NewService(Config{MonitorUSB: true}).sources); n != 0 {
		t.Errorf("disabled with USB: %d sources, want 0", n)
	}
}

// Stop stops every source even when one fails to stop.
func TestService_StopReportsSourceErrors(t *testing.T) {
	s := NewService(Config{Enabled: true})
	a := &streamSource{stopErr: errors.New("stuck")}
	b := &streamSource{}
	s.sources = append(s.sources, a, b)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if !a.stopped || !b.stopped {
		t.Errorf("stopped = %v, %v; want both", a.stopped, b.stopped)
	}
}

// A notification that cannot be published (closed bus) is dropped, not
// blocked on.
func TestSendNotification_PublishFails(t *testing.T) {
	msgBus := bus.NewMessageBus()
	msgBus.Close()
	s := NewService(Config{Enabled: true, Target: func() (string, string, bool) { return "telegram", "1", true }})
	s.SetBus(msgBus)
	s.sendNotification(context.Background(), &events.DeviceEvent{Action: events.ActionAdd, Kind: events.KindUSB})
}

func TestSendNotification_NoBus(t *testing.T) {
	called := false
	s := NewService(Config{Enabled: true, Target: func() (string, string, bool) { called = true; return "telegram", "1", true }})
	s.sendNotification(context.Background(), &events.DeviceEvent{})
	if called {
		t.Error("target consulted with no bus set")
	}
}
