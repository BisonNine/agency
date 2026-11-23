package testalerts

import (
	"context"
	"testing"

	"github.com/tenebris-tech/alerter"

	"go.bison9.com/agency/alerts"
)

func TestRecorderKeepsSentAlerts(t *testing.T) {
	r := &Recorder{}
	r.Send(alerter.Alert{Title: "one"})
	r.Normal("ignored", "x")
	r.Urgent("ignored", "x")
	r.Emergency("ignored", "x")
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.Alerts()
	if len(got) != 1 || got[0].Title != "one" {
		t.Fatalf("Alerts = %+v, want the one sent", got)
	}
	got[0].Title = "changed"
	if r.Alerts()[0].Title != "one" {
		t.Error("Alerts returned the recorder's own slice")
	}
}

func TestInstallCapturesProcessAlerts(t *testing.T) {
	var r *Recorder
	t.Run("installed", func(t *testing.T) {
		r = Install(t)
		alerts.Send(alerter.Alert{Title: "alice"})
	})
	if got := r.Alerts(); len(got) != 1 || got[0].Title != "alice" {
		t.Fatalf("Alerts = %+v", got)
	}
	// Removed when the subtest ended.
	alerts.Send(alerter.Alert{Title: "bob"})
	if got := r.Alerts(); len(got) != 1 {
		t.Errorf("alert sent after cleanup was recorded: %+v", got)
	}
}
