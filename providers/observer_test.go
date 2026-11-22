// ClawEh
// License: MIT

package providers

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"

	"go.bison9.com/agency/alerts"
	"go.bison9.com/agency/config"
)

// recorder is an Alerter that keeps what it was sent.
type recorder struct {
	mu     sync.Mutex
	alerts []alerter.Alert
}

func (r *recorder) Send(a alerter.Alert) {
	r.mu.Lock()
	r.alerts = append(r.alerts, a)
	r.mu.Unlock()
}

func (r *recorder) Normal(title, desc string, _ ...string) {
	r.Send(alerter.Alert{Priority: alerter.Normal, Title: title, Description: desc})
}

func (r *recorder) Urgent(title, desc string, _ ...string) {
	r.Send(alerter.Alert{Priority: alerter.Urgent, Title: title, Description: desc})
}

func (r *recorder) Emergency(title, desc string, _ ...string) {
	r.Send(alerter.Alert{Priority: alerter.Emergency, Title: title, Description: desc})
}
func (r *recorder) Close(context.Context) error { return nil }

func (r *recorder) got() []alerter.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alerter.Alert(nil), r.alerts...)
}

// alertStart is a fixed start for the alert tests, so the text an alert
// carries about it can be compared.
var alertStart = time.Date(2026, time.November, 14, 9, 2, 0, 0, time.UTC)

// alertRig is one model, "m" (wire "m"), on each of the providers
// "claude-cli", "openrouter" and "openai" (named "m@<provider>"), in a
// catalogue the model alerts observe, on a fake clock.
type alertRig struct {
	cat    *models.Catalog
	clk    *clock.Fake
	rec    *recorder
	alerts *ModelAlerts
	client *modelstest.Provider
}

func newAlertRig(t *testing.T) *alertRig {
	t.Helper()
	r := &alertRig{clk: clock.NewFake(alertStart), rec: &recorder{}, alerts: NewModelAlerts(), client: modelstest.New()}
	r.alerts.SetAlerter(r.rec)
	cfg := &config.Config{}
	for _, p := range []string{"claude-cli", "openrouter", "openai"} {
		cfg.Providers = append(cfg.Providers, config.Provider{Name: p, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"})
		cfg.Models = append(cfg.Models, config.ModelConfig{ModelName: "m@" + p, Model: "m", Provider: p, Enabled: true})
	}
	r.cat = NewCatalogBuilder(r.alerts, r.clk).Build(cfg)
	for _, p := range []string{"claude-cli", "openrouter", "openai"} {
		if err := r.cat.RegisterClient("m@"+p, r.client); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// call makes one call to the model on provider, answering with the failure
// reason gives, or with an answer when reason is empty. A model still parked
// is not called.
func (r *alertRig) call(t *testing.T, provider string, reason FailoverReason) {
	t.Helper()
	step := modelstest.Reply("ok")
	if reason != "" {
		step = modelstest.Status(statusFor(reason), 0)
	}
	r.client.Script("m", step)
	_, err := ModelSet(r.cat, "m@"+provider).Begin(models.Chat).Chat(context.Background(),
		Identity{Agent: "alice", Name: "Alice", Project: "default"}, models.Request{})
	if (err == nil) != (reason == "") {
		t.Fatalf("call to m@%s: err = %v, want a failure: %v", provider, err, reason != "")
	}
}

// fail fails the model on provider once, waiting out its cooldown first, so
// the call reaches it.
func (r *alertRig) fail(t *testing.T, provider string, reason FailoverReason) {
	t.Helper()
	if info, _ := r.cat.Get("m@" + provider); !info.Health.Available {
		r.clk.Advance(r.clk.Until(info.Health.Until))
	}
	r.call(t, provider, reason)
}

// An auth failure alerts on the first failure, keyed by the model, because no
// retry fixes a logged-out CLI; so does a billing failure of another model.
func TestModelAlerts_AuthAtOnce(t *testing.T) {
	r := newAlertRig(t)
	r.fail(t, "claude-cli", FailoverAuth)
	got := r.rec.got()
	if len(got) != 1 || got[0].Priority != alerter.Normal || got[0].Title != alertModelAuth ||
		got[0].EventID != ModelKey("claude-cli", "m") {
		t.Fatalf("auth failure must alert once with the model as event id, got %+v", got)
	}
	if want := "claude-cli/m parked for 30m0s after 1 consecutive failure(s): auth (status 401)"; got[0].Description != want {
		t.Fatalf("description = %q, want %q", got[0].Description, want)
	}
	r.fail(t, "openrouter", FailoverBilling)
	if got := r.rec.got(); len(got) != 2 || got[1].Title != alertModelAuth {
		t.Fatalf("billing failure of another model must alert, got %+v", got)
	}
}

// A model that stays out of credit is parked again each time its cooldown
// ends, but alerts once, then once a day with the start of the trouble and
// the count of parks held back.
func TestModelAlerts_RepeatsHeldForADay(t *testing.T) {
	r := newAlertRig(t)
	r.fail(t, "openrouter", FailoverBilling)
	held := 0
	for r.clk.Since(alertStart)+time.Hour < alerts.RemindEvery {
		r.fail(t, "openrouter", FailoverBilling)
		held++
	}
	// An auth failure of the same model is the same trouble, not a new alert.
	r.fail(t, "openrouter", FailoverAuth)
	held++
	if got := r.rec.got(); len(got) != 1 {
		t.Fatalf("re-parks inside a day must not alert again, got %d alerts", len(got))
	}

	r.clk.Advance(alertStart.Add(alerts.RemindEvery).Sub(r.clk.Now()))
	r.fail(t, "openrouter", FailoverBilling)
	got := r.rec.got()
	if len(got) != 2 || got[1].Title != alertModelAuth || got[1].EventID != ModelKey("openrouter", "m") {
		t.Fatalf("a day later the model must alert once more, got %+v", got)
	}
	want := "; ongoing since Nov 14 09:02, " + strconv.Itoa(held) + " more since the last alert"
	if !strings.HasSuffix(got[1].Description, want) {
		t.Fatalf("reminder = %q, want it to end %q", got[1].Description, want)
	}
}

// A model alerted as failing that answers again raises one "working again"
// alert, and its next failure is a new incident that alerts at once.
func TestModelAlerts_RecoveryAlertsOnceAndStartsAfresh(t *testing.T) {
	r := newAlertRig(t)
	id := ModelKey("openrouter", "m")
	r.fail(t, "openrouter", FailoverBilling)
	r.clk.Advance(2 * time.Hour)
	r.call(t, "openrouter", "")

	got := r.rec.got()
	if len(got) != 2 || got[1].Title != alertModelUp || got[1].EventID != id+"-up" || got[1].Priority != alerter.Normal {
		t.Fatalf("recovery must raise one working-again alert, got %+v", got)
	}
	if want := id + " answered again; it had been failing since Nov 14 09:02"; got[1].Description != want {
		t.Fatalf("recovery description = %q, want %q", got[1].Description, want)
	}
	r.call(t, "openrouter", "")
	if got = r.rec.got(); len(got) != 2 {
		t.Fatalf("a model that keeps working must not alert again, got %+v", got)
	}
	r.clk.Advance(time.Hour)
	r.fail(t, "openrouter", FailoverBilling)
	got = r.rec.got()
	if len(got) != 3 || got[2].Title != alertModelAuth || strings.Contains(got[2].Description, "ongoing") {
		t.Fatalf("a failure after recovery must alert as a new incident, got %+v", got)
	}
}

// A success tells the operator nothing when the model was never alerted as
// failing, whether it never failed or failed without settling.
func TestModelAlerts_NoRecoveryAlertWithoutAnAlert(t *testing.T) {
	r := newAlertRig(t)
	r.call(t, "openai", "")
	r.fail(t, "openai", FailoverRateLimit) // one transient failure: no alert
	r.fail(t, "openai", "")
	if got := r.rec.got(); len(got) != 0 {
		t.Fatalf("no alert was raised, so none may say it is over, got %+v", got)
	}
}

// Clearing a cooldown (one model or all, as the cooldown commands do) keeps
// the incident but lets the next failure alert at once; a success after the
// clear still reports recovery.
func TestModelAlerts_ClearRearmsTheAlert(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(r *alertRig)
	}{
		{"one model", func(r *alertRig) {
			r.cat.ResetHealth("m@openrouter")
			r.alerts.Rearm(ModelKey("openrouter", "m"))
		}},
		{"all models", func(r *alertRig) {
			r.cat.ResetHealth()
			r.alerts.RearmAll()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAlertRig(t)
			r.fail(t, "openrouter", FailoverBilling)

			r.clk.Advance(10 * time.Minute)
			tc.clear(r)
			r.call(t, "openrouter", FailoverBilling)
			got := r.rec.got()
			if len(got) != 2 || got[1].Title != alertModelAuth ||
				!strings.HasSuffix(got[1].Description, "; ongoing since Nov 14 09:02") {
				t.Fatalf("a failure after a clear must alert at once as ongoing, got %+v", got)
			}

			r.clk.Advance(10 * time.Minute)
			tc.clear(r)
			r.call(t, "openrouter", "")
			got = r.rec.got()
			if len(got) != 3 || got[2].Title != alertModelUp || !strings.HasSuffix(got[2].Description, "since Nov 14 09:02") {
				t.Fatalf("a success after a clear must report recovery, got %+v", got)
			}
		})
	}
}

// A model alerted as parked for rate limits that then loses its credit
// alerts for that too, and one recovery covers both, from the earlier start.
func TestModelAlerts_SettledAndAuthAreSeparateIncidents(t *testing.T) {
	r := newAlertRig(t)
	for range 4 {
		r.fail(t, "openai", FailoverRateLimit)
	}
	r.clk.Advance(time.Hour)
	r.fail(t, "openai", FailoverRateLimit) // a new park of the same incident: held
	r.fail(t, "openai", FailoverBilling)
	got := r.rec.got()
	if len(got) != 2 || got[0].Title != alertModelParked || got[1].Title != alertModelAuth {
		t.Fatalf("want the parked alert, then the billing alert, got %+v", got)
	}
	if want := "openai/m parked for 10m0s after 4 consecutive failure(s): rate_limit (status 429)"; got[0].Description != want {
		t.Fatalf("parked description = %q, want %q", got[0].Description, want)
	}
	r.fail(t, "openai", "")
	got = r.rec.got()
	// The parked incident began at the fourth failure, the cooldowns of the
	// first three (1, 3 and 5 minutes) after the start.
	if len(got) != 3 || got[2].Title != alertModelUp || !strings.HasSuffix(got[2].Description, "since Nov 14 09:11") {
		t.Fatalf("one recovery from the earlier start must cover both, got %+v", got)
	}
}

// Transient failures alert only once the escalation steps are used up.
func TestModelAlerts_TransientOnlyWhenSettled(t *testing.T) {
	r := newAlertRig(t)
	for range 3 {
		r.fail(t, "openai", FailoverRateLimit)
	}
	if got := r.rec.got(); len(got) != 0 {
		t.Fatalf("no alert during escalation, got %+v", got)
	}
	r.fail(t, "openai", FailoverRateLimit)
	got := r.rec.got()
	if len(got) != 1 || got[0].Priority != alerter.Normal || got[0].EventID != ModelKey("openai", "m") {
		t.Fatalf("settled cooldown must alert once, got %+v", got)
	}
}

// The task models' tracker reports to the same model alerts, so a model has
// one incident whichever path saw it fail.
func TestModelAlerts_SharedWithTaskModelTracker(t *testing.T) {
	r := newAlertRig(t)
	ct := NewCooldownTracker()
	ct.SetAlerts(r.alerts)
	ct.MarkFailure("openrouter", "m", FailoverBilling, 402, 0)
	r.fail(t, "openrouter", FailoverBilling)
	if got := r.rec.got(); len(got) != 1 {
		t.Fatalf("one incident across both paths, got %+v", got)
	}
	ct.MarkSuccess("openrouter", "m")
	if got := r.rec.got(); len(got) != 2 || got[1].Title != alertModelUp {
		t.Fatalf("a task-model success ends the incident, got %+v", got)
	}
}

// Without an alerter nothing is sent and the catalogue keeps working.
func TestModelAlerts_NoAlerterIsFine(t *testing.T) {
	r := newAlertRig(t)
	r.alerts.SetAlerter(nil)
	r.fail(t, "openai", FailoverAuth)
	if info, _ := r.cat.Get("m@openai"); info.Health.Available {
		t.Fatal("model should be parked")
	}
}
