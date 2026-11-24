package internal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// LoadConfig creates AGENCY_HOME on first run; the directory is owner-only.
func TestLoadConfig_CreatesOwnerOnlyHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}
	home := filepath.Join(t.TempDir(), "agency-home")
	t.Setenv("AGENCY_HOME", home)

	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat %s: %v", home, err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("AGENCY_HOME mode = %04o, want 0700", got)
	}
}
