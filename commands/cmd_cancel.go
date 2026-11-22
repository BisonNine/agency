package commands

import (
	"context"
	"fmt"
	"strings"
)

func cancelCommand() Definition {
	return Definition{
		Name:        "cancel",
		Description: "Stop the current reply and skip the messages queued behind it; /cancel all also stops the agent's background tasks",
		Usage:       "/cancel [all]",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.CancelPending == nil {
				return req.Reply(unavailableMsg)
			}
			all := strings.EqualFold(nthToken(req.Text, 1), "all")
			return req.Reply(cancelReply(rt.CancelPending(all)))
		},
	}
}

// CancelOutcome is what a /cancel did, for its reply.
type CancelOutcome struct {
	// Agent is the display name of the agent the cancel was for.
	Agent string
	// Running: a reply in progress was stopped. Skipped counts the queued
	// messages dropped.
	Running bool
	Skipped int
	// All: /cancel all. Cancelled counts the background tasks it stopped;
	// StillRunning those a plain /cancel left running.
	All          bool
	Cancelled    int
	StillRunning int
}

// cancelReply words the /cancel outcome as one message: what was stopped,
// how many queued messages were skipped, and the background tasks
// cancelled or still running.
func cancelReply(o CancelOutcome) string {
	if !o.Running && o.Skipped == 0 && o.Cancelled == 0 {
		if !o.All && o.StillRunning > 0 {
			return o.Agent + ": nothing to stop. " + stillRunning(o.StillRunning)
		}
		return "No pending messages to cancel."
	}
	var did []string
	if o.Running {
		did = append(did, "stopped")
	}
	if o.Skipped > 0 {
		did = append(did, "skipped "+count(o.Skipped, "queued message", "queued messages"))
	}
	if o.Cancelled > 0 {
		did = append(did, "cancelled "+count(o.Cancelled, "background task", "background tasks"))
	}
	text := o.Agent + ": " + joinActions(did) + "."
	if !o.All && o.StillRunning > 0 {
		text += " " + stillRunning(o.StillRunning)
	}
	return text
}

// stillRunning tells how many background tasks a plain /cancel left.
func stillRunning(n int) string {
	if n == 1 {
		return "1 background task is still running — /cancel all stops it too."
	}
	return fmt.Sprintf("%d background tasks are still running — /cancel all stops them too.", n)
}

// joinActions joins "stopped", "skipped …" and "cancelled …" as the plan's
// replies read: "stopped, and cancelled 2 background tasks".
func joinActions(did []string) string {
	switch len(did) {
	case 1:
		return did[0]
	case 2:
		return did[0] + ", and " + did[1]
	default:
		return strings.Join(did[:len(did)-1], ", ") + " and " + did[len(did)-1]
	}
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
