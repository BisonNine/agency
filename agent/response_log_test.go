// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// The "LLM response" line is one debug line under component agent, after the
// fallback's success line and the token calibration and before the direct
// answer's line, with the same fields; the reasoning only when content may
// be logged.
func TestLLMResponseLine_PlaceAndFields(t *testing.T) {
	for _, logContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("content=%v", logContent), func(t *testing.T) {
			logger.SetLogMessageContent(logContent)
			t.Cleanup(func() { logger.SetLogMessageContent(false) })
			cfg := abcConfig(t.TempDir(), "a", "b")
			al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
			p := modelScriptProvider{answer: func(_ context.Context, model string) (*providers.LLMResponse, error) {
				if model == "a-wire" {
					return nil, errors.New(rateLimited)
				}
				return &providers.LLMResponse{
					Content: "hello", Reasoning: "thought", Normal: true,
					Usage: &providers.UsageInfo{PromptTokens: 120, CompletionTokens: 3},
				}, nil
			}}
			for _, name := range []string{"a", "b"} {
				if err := al.catalog.RegisterClient(name, p); err != nil {
					t.Fatal(err)
				}
			}
			main := al.GetRegistry().Default()
			opts := processOptions{SessionKey: "s", Channel: "telegram", ChatID: "42", UserMessage: "hi"}
			cm, release := al.getContextManager(main, opts.SessionKey)
			defer release()

			buf := &bytes.Buffer{}
			restore := logger.RedirectForTest(buf)
			_, err := al.runLLMIteration(context.Background(), main, []providers.Message{{Role: "user", Content: "hi"}}, opts, cm, nil)
			restore()
			if err != nil {
				t.Fatal(err)
			}

			recs := readLogRecords(t, buf)
			var msgs []string
			at := -1
			for i, rec := range recs {
				msgs = append(msgs, fmt.Sprint(rec["message"]))
				if rec["message"] == "LLM response" {
					if at >= 0 {
						t.Fatalf("two \"LLM response\" lines: %q", msgs)
					}
					at = i
				}
			}
			if at < 2 || at+1 >= len(recs) {
				t.Fatalf("\"LLM response\" at %d of %q", at, msgs)
			}
			want := []string{
				"Fallback: succeeded with p/b-wire after 2 attempts",
				"token estimate calibrated from provider usage",
				"LLM response",
				"LLM response without tool calls (direct answer)",
			}
			if got := msgs[at-2 : at+2]; !slices.Equal(got, want) {
				t.Fatalf("lines around \"LLM response\" = %q, want %q", got, want)
			}

			line := recs[at]
			assertFields(t, line, map[string]any{
				"level": "debug", "agent_id": main.ID, "iteration": float64(1),
				"content_chars": float64(5), "tool_calls": float64(0), "reasoning_chars": float64(7),
				"target_channel": "", "channel": "telegram",
			})
			wantKeys := []string{"agent_id", "channel", "content_chars", "iteration", "reasoning_chars", "target_channel", "tool_calls"}
			if logContent {
				assertFields(t, line, map[string]any{"reasoning": "thought"})
				wantKeys = append(wantKeys, "reasoning")
				sort.Strings(wantKeys)
			}
			if c := fmt.Sprint(line["caller"]); !strings.HasPrefix(c, "agent ") {
				t.Errorf("caller = %q, want component agent", c)
			}
			var keys []string
			for k := range line {
				switch k {
				case "level", "time", "message", "caller":
				default:
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			if !slices.Equal(keys, wantKeys) {
				t.Errorf("fields = %q, want %q (line %v)", keys, wantKeys, line)
			}
		})
	}
}
