package agents

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agent/models"

	"go.bison9.com/agency/global"
)

// A nil set, spawner or manager answers safely: nothing running,
// nothing cancelled, and the spawn tools say spawn is unavailable.
func TestNilReceivers(t *testing.T) {
	var live *LiveSet
	live.Add("u")
	live.Remove("u")
	if live.Has("u") {
		t.Error("a nil set holds nothing")
	}

	var sp *Spawner
	if _, err := sp.TaskStatus("u"); err == nil || err.Error() != "spawn is not available" {
		t.Errorf("TaskStatus: %v", err)
	}
	if _, err := sp.TaskList(); err == nil || err.Error() != "spawn is not available" {
		t.Errorf("TaskList: %v", err)
	}
	if _, err := sp.CancelTask("u"); err == nil || err.Error() != "spawn is not available" {
		t.Errorf("CancelTask: %v", err)
	}
	if _, err := sp.CancelAllTasks(); err == nil || err.Error() != "spawn is not available" {
		t.Errorf("CancelAllTasks: %v", err)
	}

	var sm *SubagentManager
	ctx := context.Background()
	if _, err := sm.RunSync(ctx, "task", "", ""); !errors.Is(err, global.ErrSpawnUnavailable) {
		t.Errorf("RunSync: %v", err)
	}
	if _, err := sm.Run(ctx, "task", "", "", "", "", "", "", nil, nil); err == nil || err.Error() != "subagent manager not configured" {
		t.Errorf("Run: %v", err)
	}
}

// A blank task is refused before anything starts.
func TestBlankTaskRefused(t *testing.T) {
	sm := NewSubagentManager(SubagentManagerConfig{Workspace: t.TempDir()})
	ctx := context.Background()
	if _, err := sm.RunSync(ctx, " ", "", ""); err == nil || err.Error() != "task is required" {
		t.Errorf("RunSync: %v", err)
	}
	if _, err := sm.Run(ctx, " ", "", "", "", "", "", "", nil, nil); err == nil || err.Error() != "task is required" {
		t.Errorf("Run: %v", err)
	}
	if _, err := sm.spawnCallback(ctx, spawnOrigin{}, "\n", "n", "", "", "", "", "", nil, nil); err == nil || err.Error() != "task is required" {
		t.Errorf("spawnCallback: %v", err)
	}
}

func TestFriendlyModeAndCandidateNames(t *testing.T) {
	for in, want := range map[string]string{"wait": "wait", "callback": "background", "other": "other"} {
		if got := friendlyMode(in); got != want {
			t.Errorf("friendlyMode(%q) = %q", in, got)
		}
	}
	if got := candidateNames([]models.Candidate{{Name: "fast"}, {Model: "vendor/model-x"}}); got != "fast, vendor/model-x" {
		t.Errorf("candidateNames = %q", got)
	}
}
