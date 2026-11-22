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

// A call the agent's allow checker refuses logs one line, the host's denial
// warning naming the tool, and nothing else: the catalogue does not log the
// gate's refusal again.
func TestRegistry_DeniedCallLogsOneWarning(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{name: "probe", params: map[string]any{}, result: SilentResult("ran")})

	var buf safeBuf
	restore := logger.RedirectForTest(&buf)
	defer restore()
	ctx := WithToolAllowChecker(context.Background(), denyAllChecker{})
	res := r.ExecuteWithContext(ctx, "probe", nil, "", "", nil)
	if res == nil || !res.IsError || !IsExpectedRefusal(res.Err) {
		t.Fatalf("want a refusal, got %+v", res)
	}

	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", len(lines), buf.String())
	}
	for _, want := range []string{`"level":"warn"`, "Tool execution denied by agent allowlist", `"tool":"probe"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line lacks %s: %s", want, lines[0])
		}
	}
}
