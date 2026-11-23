package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearPendingTurn(t *testing.T) {
	dir := t.TempDir()
	sm := NewManager(dir, 0o700, 0o600)

	// Nothing recorded: a no-op that writes nothing.
	if err := sm.ClearPendingTurn("agent:alice:main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "state.json")); !os.IsNotExist(err) {
		t.Errorf("clearing nothing wrote the state file: %v", err)
	}

	if err := sm.SetPendingTurn("agent:alice:main", PendingTurn{Channel: "telegram", ChatID: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetPendingTurn("agent:bob:main", PendingTurn{Channel: "slack", ChatID: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := sm.ClearPendingTurn("agent:alice:main"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.GetPendingTurn("agent:alice:main"); ok {
		t.Error("cleared turn still present")
	}
	// The clear is saved: a fresh manager sees only Bob's turn.
	re := NewManager(dir, 0o700, 0o600)
	if _, ok := re.GetPendingTurn("agent:alice:main"); ok {
		t.Error("cleared turn reloaded from disk")
	}
	if got, ok := re.GetPendingTurn("agent:bob:main"); !ok || got.ChatID != "2" {
		t.Errorf("Bob's turn = %+v, %v", got, ok)
	}
}

func TestSetPendingTurn_KeepsAttempts(t *testing.T) {
	sm := NewManager(t.TempDir(), 0o700, 0o600)
	key := "agent:alice:main"
	if err := sm.SetPendingTurn(key, PendingTurn{Channel: "telegram", Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetPendingTurn(key, PendingTurn{Channel: "telegram", ChatID: "9"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := sm.GetPendingTurn(key); got.Attempts != 2 || got.ChatID != "9" {
		t.Errorf("after re-set = %+v, want Attempts 2 kept", got)
	}
}

// The state file being unwritable makes Set and Clear report the failure.
func TestPendingTurn_SaveFailure(t *testing.T) {
	dir := t.TempDir()
	sm := NewManager(dir, 0o700, 0o600)
	if err := sm.SetPendingTurn("k", PendingTurn{Channel: "telegram"}); err != nil {
		t.Fatal(err)
	}
	// Replace the state file with a non-empty directory so the rename fails.
	stateFile := filepath.Join(dir, "state", "state.json")
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateFile, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetPendingTurn("k2", PendingTurn{Channel: "slack"}); err == nil {
		t.Error("SetPendingTurn: want save error")
	}
	if err := sm.ClearPendingTurn("k"); err == nil {
		t.Error("ClearPendingTurn: want save error")
	}
}

func TestNewManager_MigratesOldStateFile(t *testing.T) {
	dir := t.TempDir()
	old := `{"pending_turns":{"agent:alice:main":{"channel":"telegram","chat_id":"7"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	sm := NewManager(dir, 0o700, 0o600)
	if got, ok := sm.GetPendingTurn("agent:alice:main"); !ok || got.ChatID != "7" {
		t.Fatalf("migrated turn = %+v, %v", got, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "state.json")); err != nil {
		t.Errorf("migrated state not written to the new location: %v", err)
	}
}

func TestNewManager_UnreadableState(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state", "state.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		sm := NewManager(dir, 0o700, 0o600)
		if _, ok := sm.GetPendingTurn("anything"); ok {
			t.Error("corrupt state produced a pending turn")
		}
		// The manager still works and overwrites the corrupt file.
		if err := sm.SetPendingTurn("k", PendingTurn{Channel: "telegram"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("state file is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "state", "state.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		sm := NewManager(dir, 0o700, 0o600)
		if !sm.GetTimestamp().IsZero() {
			t.Error("unreadable state produced a timestamp")
		}
	})
	t.Run("old state corrupt is ignored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		NewManager(dir, 0o700, 0o600)
		if _, err := os.Stat(filepath.Join(dir, "state", "state.json")); !os.IsNotExist(err) {
			t.Errorf("corrupt old state was migrated: %v", err)
		}
	})
}

func TestNewManager_MigrationSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	old := `{"pending_turns":{"k":{"channel":"telegram","chat_id":"7"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// A read-only workspace: the state dir cannot be made, so the migration
	// save fails, but the migrated turn is still in memory.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	sm := NewManager(dir, 0o700, 0o600)
	if got, ok := sm.GetPendingTurn("k"); !ok || got.ChatID != "7" {
		t.Fatalf("migrated turn = %+v, %v", got, ok)
	}
}
