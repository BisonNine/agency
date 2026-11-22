// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// The summarization caller's own lines are today's: a parked model is
// skipped with "compression model in cooldown; skipping", a failed one with
// "compression model call failed; trying next in chain" (component
// llmcontext, the same fields), and each call writes its "LLM finish" line
// naming the summarization model, its provider and the session.
func TestCompressModelCaller_LogLines(t *testing.T) {
	parked := &scriptedProvider{content: "never"}
	broken := &scriptedProvider{err: errors.New("boom")}
	good := &scriptedProvider{content: "ok"}
	cat := compressCatalog(map[string]providers.LLMProvider{"parked": parked, "broken": broken, "good": good})
	parkFor(t, cat, "parked", time.Minute, parked)
	c := compressCaller(cat, "parked", "broken", "good")
	c.sessionKey = "agent:alice:main"

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := c.Complete(withTurnID(context.Background(), "feed0002"), ctxengine.ModelRequest{System: "s", User: "u"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	lines := readLogRecords(t, buf)
	fieldsOf := func(rec map[string]any) []string {
		keys := slices.Sorted(maps.Keys(rec))
		return keys
	}
	byMsg := func(msg string) map[string]any {
		for _, rec := range lines {
			if fmt.Sprint(rec["message"]) == msg {
				return rec
			}
		}
		t.Fatalf("no %q line in %v", msg, lines)
		return nil
	}
	skip := byMsg("compression model in cooldown; skipping")
	// The line's component is the first word of its caller field.
	component := func(rec map[string]any) string { return strings.Fields(fmt.Sprint(rec["caller"]))[0] }
	if component(skip) != "llmcontext" || !slices.Equal(fieldsOf(skip), []string{"agent_id", "caller", "level", "message", "model", "remaining", "session", "time"}) {
		t.Errorf("skip line = %v", skip)
	}
	assertFields(t, skip, map[string]any{
		"level": "info", "agent_id": "alice", "session": "agent:alice:main", "model": "parked",
	})
	if r := fmt.Sprint(skip["remaining"]); r != "1m0s" && r != "59s" {
		t.Errorf("remaining = %q, want about a minute", r)
	}
	failed := byMsg("compression model call failed; trying next in chain")
	if component(failed) != "llmcontext" || !slices.Equal(fieldsOf(failed), []string{"agent_id", "caller", "error", "level", "message", "model", "session", "time"}) {
		t.Errorf("failure line = %v", failed)
	}
	assertFields(t, failed, map[string]any{
		"level": "warn", "agent_id": "alice", "session": "agent:alice:main", "model": "broken", "error": "boom",
	})
	fin := finishRecords(t, buf)
	if len(fin) != 2 {
		t.Fatalf("finish lines = %d, want one per call (2): %v", len(fin), fin)
	}
	assertFields(t, fin[1], map[string]any{
		"kind": "compression", "agent_id": "alice", "our_provider": "p-good", "our_model": "good",
		"model": "good", "session_key": "agent:alice:main", "turn_id": "feed0002", "success": true,
	})
}
