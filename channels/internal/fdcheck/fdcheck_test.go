package fdcheck

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenFor(t *testing.T) {
	if runtime.GOOS != "linux" {
		open, ok := OpenFor(t.TempDir())
		if open || ok {
			t.Fatalf("OpenFor on %s = (%v, %v), want (false, false)", runtime.GOOS, open, ok)
		}
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "alice.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bob.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		path     string
		hold     bool
		wantOpen bool
	}{
		{name: "closed file", path: path, hold: false, wantOpen: false},
		{name: "open file", path: path, hold: true, wantOpen: true},
		{name: "open through symlink", path: link, hold: true, wantOpen: true},
		{name: "missing path", path: filepath.Join(dir, "missing"), hold: false, wantOpen: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hold {
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
			}
			open, ok := OpenFor(tt.path)
			if !ok {
				t.Fatal("OpenFor ok = false on linux")
			}
			if open != tt.wantOpen {
				t.Errorf("OpenFor(%q) open = %v, want %v", tt.path, open, tt.wantOpen)
			}
		})
	}
}
