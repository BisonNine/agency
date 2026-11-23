// ClawEh
// License: MIT

package agent

import (
	"context"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
)

// sessToolsCovLoop is a loop over the test config and its default agent.
func sessToolsCovLoop(t *testing.T) (*AgentLoop, *AgentInstance) {
	t.Helper()
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	return al, agent
}

// TestBuildSessionInfo_ArchiveSummaryAndCompaction: session_info reports the
// context's message count, the archive's sequence range, how many messages
// it holds and when the first was written, the summary's covered range and
// when the session was last compacted.
func TestBuildSessionInfo_ArchiveSummaryAndCompaction(t *testing.T) {
	al, agent := sessToolsCovLoop(t)
	const key = "agent:main:info"
	compacted := time.Date(2026, time.November, 20, 8, 30, 0, 0, time.UTC)

	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		t.Fatalf("session store %T does not keep compaction state", agent.Sessions)
	}
	if err := store.SetCompactionState(key, memory.CompactionState{SummaryGeneratedAt: compacted}); err != nil {
		t.Fatal(err)
	}
	setSummary(t, agent.Sessions, key, `{"covered_seq_start":1,"covered_seq_end":2,"text":"Alice asked Bob"}`)

	cm, release := al.getContextManager(agent, key)
	for _, text := range []string{"hello", "again", "third"} {
		if _, err := cm.AddUserMessage(context.Background(), spawnllm.Message{Role: "user", Content: text}); err != nil {
			release()
			t.Fatalf("AddUserMessage: %v", err)
		}
	}
	release()

	info, err := buildSessionInfo(al, agent, key)
	if err != nil {
		t.Fatalf("buildSessionInfo: %v", err)
	}
	if info.SessionKey != key || info.ContextMessageCount != 3 {
		t.Fatalf("info = %+v, want key %q and 3 context messages", info, key)
	}
	if info.ArchiveMinSeq != 1 || info.ArchiveMaxSeq != 3 || info.TotalArchived != 3 {
		t.Fatalf("archive = %d..%d (%d), want 1..3 (3)", info.ArchiveMinSeq, info.ArchiveMaxSeq, info.TotalArchived)
	}
	if info.StartedAt == nil || info.StartedAt.IsZero() {
		t.Fatalf("StartedAt = %v, want the first archived message's time", info.StartedAt)
	}
	if info.LastCompressedAt == nil || !info.LastCompressedAt.Equal(compacted) {
		t.Fatalf("LastCompressedAt = %v, want %v", info.LastCompressedAt, compacted)
	}
	sc := info.SummaryCovers
	if sc == nil || sc.SeqStart != 1 || sc.SeqEnd != 2 || sc.GeneratedAt == nil || !sc.GeneratedAt.Equal(compacted) {
		t.Fatalf("SummaryCovers = %+v, want 1..2 generated at %v", sc, compacted)
	}
}

// TestBuildSessionInfo_NewSession: a session with no archive, no compaction
// and a summary that names no covered range reports none of them.
func TestBuildSessionInfo_NewSession(t *testing.T) {
	for _, tc := range []struct {
		name, summary string
	}{
		{"no summary", ""},
		{"summary without a range", `{"text":"nothing covered"}`},
		{"summary not JSON", "plain text summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, agent := sessToolsCovLoop(t)
			const key = "agent:main:fresh"
			if tc.summary != "" {
				setSummary(t, agent.Sessions, key, tc.summary)
			}
			info, err := buildSessionInfo(al, agent, key)
			if err != nil {
				t.Fatalf("buildSessionInfo: %v", err)
			}
			if info.ContextMessageCount != 0 || info.ArchiveMinSeq != 0 || info.ArchiveMaxSeq != 0 ||
				info.TotalArchived != 0 || info.StartedAt != nil || info.LastCompressedAt != nil || info.SummaryCovers != nil {
				t.Fatalf("info = %+v, want an empty session", info)
			}
		})
	}
}
