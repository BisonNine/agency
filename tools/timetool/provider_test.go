package timetool

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/toolspec"
)

// The provider publishes time_now, on by default, answering from the
// system clock.
func TestGlobalProvider(t *testing.T) {
	if GlobalProvider.Namespace() != "time" || GlobalProvider.Description() != "Current date, time, and timezone" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	defs := GlobalProvider.RegisterTools(toolspec.Deps{})
	if len(defs) != 1 || defs[0].Name != "now" || defs[0].DefaultAllow == nil || !*defs[0].DefaultAllow ||
		!strings.HasPrefix(defs[0].Description, "Get the current date, time, and timezone.") {
		t.Fatalf("definitions: %+v", defs)
	}
	before := time.Now().Unix()
	res, err := defs[0].Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"timezone": "UTC"}})
	after := time.Now().Unix()
	if err != nil || res.IsError || !strings.Contains(res.ForLLM, "timezone: UTC (UTC+00:00)") {
		t.Fatalf("now: %v %+v", err, res)
	}
	// The epoch reported lies between the two readings of the clock.
	i := strings.Index(res.ForLLM, "unix: ")
	var unix int64
	for _, c := range res.ForLLM[i+len("unix: "):] {
		if c < '0' || c > '9' {
			break
		}
		unix = unix*10 + int64(c-'0')
	}
	if i < 0 || unix < before || unix > after {
		t.Errorf("unix %d not in [%d, %d]", unix, before, after)
	}
}
