package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agency/logger"
)

// An expected refusal is logged as a warning; a genuine failure as an error.
func TestRegistry_RefusalLoggedAtWarn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    *ToolResult
		wantLevel string
		wantMsg   string
	}{
		{"refusal", ErrorResult("You may not message Bob.").WithError(Refusal(errors.New("not allowed"))), `"level":"warn"`, "Tool call refused"},
		{"failure", ErrorResult("disk full").WithError(errors.New("disk full")), `"level":"error"`, "Tool execution failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf safeBuf
			restore := logger.RedirectForTest(&buf)
			defer restore()

			r := NewToolRegistry()
			r.Register(&mockRegistryTool{name: "probe", params: map[string]any{}, result: tc.result})
			r.ExecuteWithContext(context.Background(), "probe", nil, "", "", nil)

			var line string
			for l := range strings.SplitSeq(buf.String(), "\n") {
				if strings.Contains(l, tc.wantMsg) {
					line = l
				}
			}
			if line == "" || !strings.Contains(line, tc.wantLevel) {
				t.Fatalf("want %q at %s, got log:\n%s", tc.wantMsg, tc.wantLevel, buf.String())
			}
		})
	}
}
