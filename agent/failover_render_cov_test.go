// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/logger"
)

// A billing failure is rendered as the billing notice, not the generic error.
func TestFailoverRenderCov_BillingError(t *testing.T) {
	err := &spawnllm.FailoverError{Reason: spawnllm.FailoverBilling, Status: 402, Provider: "p", Model: "m", Wrapped: errors.New("payment required")}
	got := renderTurnErrorText(context.Background(), time.Minute, err)
	if got == "" || strings.HasPrefix(got, "Error processing message") || got != renderBillingError(err) {
		t.Fatalf("renderTurnErrorText = %q, want the billing notice", got)
	}
}

// A fallback notice the bus refuses is logged.
func TestFailoverRenderCov_NoticePublishFails(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, msgBus := hooksCovLoop(t)
	msgBus.Close()
	notify := al.fallbackNotifier(context.Background(), processOptions{Channel: "telegram", ChatID: "42"})
	notify([]models.Attempt{{Name: "a", Err: errors.New("boom"), Reason: spawnllm.FailoverTimeout}}, models.Candidate{Name: "b"})
	if !strings.Contains(logs.String(), "Failed to publish fallback notice") {
		t.Fatalf("the refused notice was not logged: %s", logs.String())
	}
}

// The small renderers' edge cases: no error, an unnamed model, a body with
// no message, an error with no status and a budget of whole and part
// minutes.
func TestFailoverRenderCov_Edges(t *testing.T) {
	if got := renderFailoverError(nil); got != "" {
		t.Fatalf("renderFailoverError(nil) = %q", got)
	}
	declined := &models.CLIDeclinedError{Message: "allow the tool in settings"}
	if got := attemptText("", declined, ""); got != "model error: allow the tool in settings" {
		t.Fatalf("attemptText = %q", got)
	}
	if got := jsonErrorMessage(`{"code":7}`); got != "" {
		t.Fatalf("jsonErrorMessage = %q, want none", got)
	}
	if got := failoverStatus(errors.New("plain")); got != 0 {
		t.Fatalf("failoverStatus = %d, want 0", got)
	}
	if got := attemptDescription("", 0, spawnllm.FailoverTimeout); !strings.HasPrefix(got, "model error: ") {
		t.Fatalf("attemptDescription = %q", got)
	}
	if got := formatBudget(90 * time.Second); got != "1m30s" {
		t.Fatalf("formatBudget = %q, want 1m30s", got)
	}
}
