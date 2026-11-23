// ClawEh
// License: MIT

package agent

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
)

// A spawn of an unknown agent, or with media that cannot be resolved, is
// refused before any clone exists.
func TestSubagentRunCov_Refusals(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := newSubagentTestLoop(t, &mockProvider{})
	ctx := context.Background()
	for _, tt := range []struct {
		name, agent string
		store       bool
		want        string
	}{
		{"unknown agent", "nobody", false, `subagent: agent "nobody" not found`},
		{"no media store", "main", false, "subagent: media refs passed but no media store is configured"},
		{"unknown ref", "main", true, "subagent: media ref media://missing not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.store {
				al.SetMediaStore(media.NewFileMediaStore())
			}
			res, release, err := al.runSubagentTask(ctx, tt.agent, "task", "", "", []string{"media://missing"})
			release()
			if res != nil || err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("runSubagentTask = %v, %v; want %q", res, err, tt.want)
			}
			if n := len(al.GetRegistry().List()); n != 1 {
				t.Fatalf("agents = %d, want no clone", n)
			}
		})
	}
}

// A worker without a session store wrote nothing.
func TestSubagentRunCov_PartialOutputWithoutStore(t *testing.T) {
	if got := partialOutput(&AgentInstance{ID: "w"}, "s"); got != "" {
		t.Fatalf("partialOutput = %q, want none", got)
	}
}
