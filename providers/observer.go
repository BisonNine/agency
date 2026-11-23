// ClawEh
// License: MIT

package providers

import (
	"fmt"
	"sync"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/alerts"
)

// Titles of the model alerts, and the prefixes that key each one's incident
// by model: a model parked for rate limits that then loses its credit is a
// second problem, and alerts as one.
const (
	alertModelAuth   = "Model authentication or billing failure"
	alertModelParked = "Model parked after repeated failures"
	alertModelUp     = "Model working again"

	incidentAuth   = "auth\x00"
	incidentParked = "parked\x00"
)

// ModelAlerts is the catalogue's models.Observer: it raises the operator
// alerts about model health. An authentication or billing failure (a CLI
// logged out, a key revoked, credit exhausted) is reported at once, because
// no retry fixes it. Anything else is reported only once the escalation has
// settled into the category cooldown, so a single transient error pages
// nobody.
//
// A model that stays broken is parked again each time its cooldown ends and
// something tries it, so the alert is raised once when that starts and then
// at most once every alerts.RemindEvery, saying since when and how many parks
// were held back. The incident ends when the model answers (Recovered).
//
// One ModelAlerts lives as long as the process: every catalogue built (at
// start and on each reload) reports to it, so incidents outlive Adopt.
type ModelAlerts struct {
	mu      sync.RWMutex
	alerter alerter.Alerter
	// incidents holds each model's alerts to one when it starts failing and
	// one a day while it keeps failing.
	incidents alerts.Incidents
}

// NewModelAlerts returns an observer with no alerter: it alerts nobody until
// SetAlerter.
func NewModelAlerts() *ModelAlerts { return &ModelAlerts{} }

// SetAlerter routes the model alerts to a.
func (o *ModelAlerts) SetAlerter(a alerter.Alerter) {
	o.mu.Lock()
	o.alerter = a
	o.mu.Unlock()
}

func (o *ModelAlerts) current() alerter.Alerter {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.alerter
}

// Parked implements models.Observer.
func (o *ModelAlerts) Parked(e models.Parked) {
	o.parked(e.Key, e.Reason, e.Status, e.Failures, e.Settled, e.Cooldown, e.At)
}

// parked raises the alert for one park of the model keyed id, when it is
// one to raise.
func (o *ModelAlerts) parked(id string, reason spawnllm.FailoverReason, status, failures int, settled bool, cooldown time.Duration, now time.Time) {
	a := o.current()
	if a == nil {
		return
	}
	var title, incident string
	switch {
	case reason == spawnllm.FailoverAuth || reason == spawnllm.FailoverBilling:
		title, incident = alertModelAuth, incidentAuth+id
	case settled:
		title, incident = alertModelParked, incidentParked+id
	default:
		return
	}
	occ := o.incidents.Report(incident, now)
	if !occ.Alert {
		return
	}
	a.Send(alerter.Alert{
		Title: title,
		Description: fmt.Sprintf("%s parked for %s after %d consecutive failure(s): %s (status %d)%s",
			id, cooldown.Round(time.Second), failures, reason, status, occ.Note()),
		EventID: id,
	})
}

// Recovered implements models.Observer.
func (o *ModelAlerts) Recovered(e models.Recovered) { o.recovered(e.Key) }

// recovered ends the incidents of the model keyed id, which has just
// answered. When it had been alerted as failing, the operator is told once
// that it works again, so nothing is left for them to do; a failure after
// that is a new incident and alerts at once.
func (o *ModelAlerts) recovered(id string) {
	since, alerted := o.incidents.Resolve(incidentAuth + id)
	if parkedSince, parked := o.incidents.Resolve(incidentParked + id); parked && (!alerted || parkedSince.Before(since)) {
		since, alerted = parkedSince, true
	}
	a := o.current()
	if !alerted || a == nil {
		return
	}
	a.Send(alerter.Alert{
		Title:       alertModelUp,
		Description: id + " answered again; it had been failing since " + alerts.Stamp(since),
		EventID:     id + "-up",
	})
}

// FellBack implements models.Observer. A fallback is not an alert: the turn
// that fell back logs it and tells its chat (the chain's notify hook).
func (o *ModelAlerts) FellBack(models.FellBack) {}

// Exhausted implements models.Observer. Like FellBack, it is reported by the
// turn whose call it ended.
func (o *ModelAlerts) Exhausted(models.Exhausted) {}

// Rearm keeps the incidents of the models keyed ids open but lets the next
// failure alert at once: the operator cleared their cooldowns to have them
// tried again, and must hear at once if that did not help. A success still
// reports recovery.
func (o *ModelAlerts) Rearm(ids ...string) {
	for _, id := range ids {
		o.incidents.Rearm(incidentAuth + id)
		o.incidents.Rearm(incidentParked + id)
	}
}

// RearmAll is Rearm for every model.
func (o *ModelAlerts) RearmAll() { o.incidents.RearmAll() }
