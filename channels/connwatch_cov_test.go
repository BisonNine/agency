package channels

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/clock"
)

func TestConnDownSince(t *testing.T) {
	bc, _, fc := newWatchedChannel(t)
	if !bc.ConnDownSince().IsZero() {
		t.Fatal("a working connection reports an outage")
	}
	start := fc.Now()
	bc.ReportConnFailure(errors.New("down"))
	fc.Advance(time.Minute)
	bc.ReportConnFailure(errors.New("still down"))
	if got := bc.ConnDownSince(); !got.Equal(start) {
		t.Fatalf("ConnDownSince = %v, want the first failure %v", got, start)
	}
	bc.ReportConnected()
	if !bc.ConnDownSince().IsZero() {
		t.Fatal("outage still reported after reconnecting")
	}
}

// A timer of an outage that has since ended (another one began), or one
// already alerted, raises nothing.
func TestConnDownExpired_StaleTimer(t *testing.T) {
	bc, rec, fc := newWatchedChannel(t)
	old := fc.Now()
	fc.Advance(time.Minute)
	bc.ReportConnFailure(errors.New("down"))

	bc.connDownExpired(old, ConnDownAlertAfter)
	if n := recorded(rec); n != 0 {
		t.Fatalf("stale timer raised %d alerts", n)
	}

	waitPastAlert(fc)
	if n := recorded(rec); n != 1 {
		t.Fatalf("want the down alert, got %d alerts", n)
	}
	bc.connDownExpired(bc.ConnDownSince(), ConnDownAlertAfter)
	if n := recorded(rec); n != 1 {
		t.Fatalf("an alerted outage alerted again: %d alerts", n)
	}
}

// A channel found stopped when its outage timer fires (it stopped while the
// timer was firing) ends the outage without an alert.
func TestConnDownExpired_StoppedWhileFiring(t *testing.T) {
	bc, rec, fc := newWatchedChannel(t)
	bc.ReportConnFailure(errors.New("down"))
	bc.running.Store(false) // SetRunning(false) not yet past connStopped
	waitPastAlert(fc)

	if n := recorded(rec); n != 0 {
		t.Fatalf("a stopped channel alerted: %+v", snapshot(rec))
	}
	if !bc.ConnDownSince().IsZero() {
		t.Fatal("outage not ended for a stopped channel")
	}
}

func TestConnAggregator_ExpiredUnknownChannel(t *testing.T) {
	g := newConnAggregator()
	bc := NewBaseChannel("telegram-a", nil, nil, nil)
	if a, affected := g.expired(bc, ConnDownAlertAfter); a != nil || affected != nil {
		t.Fatalf("expired for a channel with no outage = %+v %v", a, affected)
	}
}

// A channel whose outage outlasts the window after its platform's down alert
// joins that alert silently and is named in the up alert.
func TestConnWatch_LateChannelJoinsPlatformOutage(t *testing.T) {
	g, rec, fc := newConnAggregator(), &alertRecorder{}, clock.NewFake(time.Now())
	a := newPlatformChannel(t, "telegram-a", g, rec, fc)
	b := newPlatformChannel(t, "telegram-b", g, rec, fc)
	a.ReportConnFailure(errors.New("down"))
	fc.Advance(ConnDownAlertAfter / 2)
	b.ReportConnFailure(errors.New("down"))
	fc.Advance(ConnDownAlertAfter / 2) // a's window ends
	if alerts := snapshot(rec); len(alerts) != 1 || alerts[0].Title != "Telegram down: telegram-a, telegram-b" {
		t.Fatalf("want one down alert naming both, got %+v", alerts)
	}
	fc.Advance(ConnDownAlertAfter / 2) // b's window ends
	if n := recorded(rec); n != 1 {
		t.Fatalf("the late channel alerted again: %+v", snapshot(rec))
	}

	a.ReportConnected()
	if n := recorded(rec); n != 1 {
		t.Fatalf("up alert while telegram-b is still down: %+v", snapshot(rec))
	}
	b.ReportConnected()
	alerts := snapshot(rec)
	if len(alerts) != 2 || !strings.HasPrefix(alerts[1].Title, "Telegram up: telegram-a, telegram-b") {
		t.Fatalf("want the up alert naming both, got %+v", alerts)
	}
}

func TestPlatformDisplayName(t *testing.T) {
	for id, want := range map[string]string{"telegram": "Telegram", "": "", "irc": "Irc"} {
		if got := platformDisplayName(id); got != want {
			t.Errorf("platformDisplayName(%q) = %q, want %q", id, got, want)
		}
	}
}

// The down alert's outage starts at the earliest failure of the platform's
// channels, whichever channel's window ends first.
func TestConnAggregator_ExpiredSinceEarliestFailure(t *testing.T) {
	g := newConnAggregator()
	a := NewBaseChannel("telegram-a", nil, nil, nil)
	b := NewBaseChannel("telegram-b", nil, nil, nil)
	t0 := time.Unix(1000, 0)
	g.failure(a, t0, "down")
	g.failure(b, t0.Add(time.Minute), "down")

	alert, affected := g.expired(b, ConnDownAlertAfter)
	if alert == nil || len(affected) != 2 {
		t.Fatalf("expired = %+v %v, want the down alert for both", alert, affected)
	}
	if got := g.platforms["telegram"].since; !got.Equal(t0) {
		t.Fatalf("outage since %v, want the earliest failure %v", got, t0)
	}
}
