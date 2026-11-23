package files

import (
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
)

// A Writer writes where file_write would and refuses, with the same words,
// where it would not; an unrestricted Writer anchors a relative path at the
// workspace.
func TestWriter(t *testing.T) {
	if w := NewWriter(nil, "/ws"); w.Folder() != "" {
		t.Errorf("no config: folder %q", w.Folder())
	}

	ws := t.TempDir()
	cfg := config.DefaultConfig()
	w := NewWriter(cfg, ws)
	if w.Folder() != "files" {
		t.Errorf("default folder %q", w.Folder())
	}
	if err := w.WriteFile("files/a/b.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "files", "a", "b.md")); err != nil || string(got) != "x" {
		t.Errorf("written: %q %v", got, err)
	}
	err := w.WriteFile("tasks/c.md", []byte("x"), 0o600)
	want := NewWriteFileToolScoped(ws, true, "files").Execute(t.Context(), map[string]any{"path": "tasks/c.md", "content": "x"})
	if !toolkit.IsExpectedRefusal(err) || err.Error() != want.ForLLM {
		t.Errorf("outside the write folder: %v, want %q", err, want.ForLLM)
	}

	cfg.Agents.Defaults.RestrictToWorkspace = false
	if err := NewWriter(cfg, ws).WriteFile("d.md", []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "d.md")); err != nil || string(got) != "y" {
		t.Errorf("unrestricted: %q %v", got, err)
	}
}
