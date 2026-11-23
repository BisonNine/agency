package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
)

// An expected refusal is logged as a warning; a genuine failure as an error.
func TestRegistry_RefusalLoggedAtWarn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    *toolkit.Result
		wantLevel string
		wantMsg   string
	}{
		{"refusal", toolkit.ErrorResult("You may not message Bob.").WithError(toolkit.Refusal(errors.New("not allowed"))), `"level":"warn"`, "Tool call refused"},
		{"failure", toolkit.ErrorResult("disk full").WithError(errors.New("disk full")), `"level":"error"`, "Tool execution failed"},
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

// A refused call logs what the host logged before the tool core moved to the
// SDK: the start line, then the refusal, and nothing else. A call the agent's
// allow checker denies is refused with the denial warning (no second
// "Tool call refused"); an unknown tool with "Tool not found".
func TestRegistry_RefusedCallLogs(t *testing.T) {
	type line struct {
		level, message, tool string
	}
	for _, tc := range []struct {
		name string
		call string
		ctx  context.Context
		want []line
	}{
		{"denied", "probe", WithToolAllowChecker(context.Background(), denyAllChecker{}), []line{
			{"info", "Tool execution started", "probe"},
			{"warn", "Tool execution denied by agent allowlist", "probe"},
		}},
		{"not found", "missing", context.Background(), []line{
			{"info", "Tool execution started", "missing"},
			{"error", "Tool not found", "missing"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewToolRegistry()
			r.Register(&mockRegistryTool{name: "probe", params: map[string]any{}, result: toolkit.SilentResult("ran")})

			var buf safeBuf
			restore := logger.RedirectForTest(&buf)
			defer restore()
			res := r.ExecuteWithContext(tc.ctx, tc.call, nil, "", "", nil)
			if res == nil || !res.IsError {
				t.Fatalf("want a refusal, got %+v", res)
			}

			var got []line
			for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(l), &rec); err != nil {
					t.Fatalf("log line %q: %v", l, err)
				}
				delete(rec, "caller")
				delete(rec, "time")
				if len(rec) != 3 {
					t.Errorf("fields %v, want level, message and tool only", rec)
				}
				got = append(got, line{fmt.Sprint(rec["level"]), fmt.Sprint(rec["message"]), fmt.Sprint(rec["tool"])})
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("log = %+v, want %+v", got, tc.want)
			}
		})
	}
}
