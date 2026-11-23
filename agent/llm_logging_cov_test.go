// ClawEh
// License: MIT

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/logger"
)

// A dump whose input or output cannot be encoded, or whose directory cannot
// be made, is not written and the failure is logged.
func TestLLMLoggingCov_DumpFailures(t *testing.T) {
	bad := []json.RawMessage{json.RawMessage("{not json")}
	goodMsgs := []spawnllm.Message{{Role: "user", Content: "hi"}}
	badMsgs := []spawnllm.Message{{Role: "assistant", ResponsesReasoning: bad}}
	goodResp := &spawnllm.LLMResponse{Content: "no", FinishReason: "refusal"}
	badResp := &spawnllm.LLMResponse{ResponsesReasoning: bad}

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unwritable := dump.Dir{Path: filepath.Join(blocker, "dumps")}

	for _, tt := range []struct {
		name     string
		dir      dump.Dir
		msgs     []spawnllm.Message
		resp     *spawnllm.LLMResponse
		refusal  string
		dumpAllW string
	}{
		{"input", dump.Dir{Path: t.TempDir()}, badMsgs, goodResp, "Failed to marshal refusal dump input", "Failed to marshal dump_all input"},
		{"output", dump.Dir{Path: t.TempDir()}, goodMsgs, badResp, "Failed to marshal refusal dump output", "Failed to marshal dump_all output"},
		{"write", unwritable, goodMsgs, goodResp, "Failed to write refusal dump", "Failed to write dump_all file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := &safeBufLoop{}
			t.Cleanup(logger.RedirectForTest(logs))
			al := &AgentLoop{}
			al.SetDumpsDir(tt.dir)
			agent := &AgentInstance{ID: "alice"}
			opts := processOptions{SessionKey: "s", Channel: "telegram"}
			al.dumpRefusal(agent, tt.msgs, tt.resp, opts, "m", 1)
			al.dumpAll(agent, tt.msgs, tt.resp, opts, "m", 1)
			for _, want := range []string{tt.refusal, tt.dumpAllW} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("%q was not logged: %s", want, logs.String())
				}
			}
			if entries, err := os.ReadDir(tt.dir.Path); err == nil && len(entries) != 0 {
				t.Fatalf("dump files were written: %v", entries)
			}
		})
	}
}
