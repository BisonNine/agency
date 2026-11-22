package commands

import (
	"context"
	"testing"
)

func TestCancelCommand_Reply(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		got  CancelOutcome
		want string
	}{
		{
			"stopped, tasks left", "/cancel",
			CancelOutcome{Agent: "Alice", Running: true, StillRunning: 2},
			"Alice: stopped. 2 background tasks are still running — /cancel all stops them too.",
		},
		{
			"stopped, one task left", "/cancel",
			CancelOutcome{Agent: "Alice", Running: true, StillRunning: 1},
			"Alice: stopped. 1 background task is still running — /cancel all stops it too.",
		},
		{"stopped, nothing left", "/cancel", CancelOutcome{Agent: "Alice", Running: true}, "Alice: stopped."},
		{
			"stopped and queued", "/cancel",
			CancelOutcome{Agent: "Alice", Running: true, Skipped: 2},
			"Alice: stopped, and skipped 2 queued messages.",
		},
		{"queued only", "/cancel", CancelOutcome{Agent: "Alice", Skipped: 1}, "Alice: skipped 1 queued message."},
		{"nothing", "/cancel", CancelOutcome{Agent: "Alice"}, "No pending messages to cancel."},
		{
			"nothing but tasks", "/cancel",
			CancelOutcome{Agent: "Alice", StillRunning: 1},
			"Alice: nothing to stop. 1 background task is still running — /cancel all stops it too.",
		},
		{
			"nothing but tasks, plural", "/cancel",
			CancelOutcome{Agent: "Alice", StillRunning: 2},
			"Alice: nothing to stop. 2 background tasks are still running — /cancel all stops them too.",
		},
		{
			"all", "/cancel all",
			CancelOutcome{Agent: "Alice", Running: true, All: true, Cancelled: 2},
			"Alice: stopped, and cancelled 2 background tasks.",
		},
		{
			"all with queue", "/cancel all",
			CancelOutcome{Agent: "Alice", Running: true, Skipped: 1, All: true, Cancelled: 1},
			"Alice: stopped, skipped 1 queued message and cancelled 1 background task.",
		},
		{
			"all, tasks only", "/cancel ALL",
			CancelOutcome{Agent: "Alice", All: true, Cancelled: 3},
			"Alice: cancelled 3 background tasks.",
		},
		{"all, nothing", "/cancel all", CancelOutcome{Agent: "Alice", All: true}, "No pending messages to cancel."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAll bool
			rt := &Runtime{CancelPending: func(all bool) CancelOutcome { gotAll = all; return tc.got }}
			var got string
			err := cancelCommand().Handler(context.Background(), Request{Text: tc.text, Reply: func(s string) error { got = s; return nil }}, rt)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if got != tc.want {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
			if gotAll != tc.got.All {
				t.Fatalf("all = %v, want %v", gotAll, tc.got.All)
			}
		})
	}
}

func TestCancelCommand_Unavailable(t *testing.T) {
	var got string
	if err := cancelCommand().Handler(context.Background(), Request{Reply: func(s string) error { got = s; return nil }}, &Runtime{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got != unavailableMsg {
		t.Fatalf("reply = %q, want %q", got, unavailableMsg)
	}
}
