package logger

import (
	"os"
	"path/filepath"
	"testing"
)

// Once file logging is disabled no log file is active: GetLogFilePath reports
// none and a roll neither archives nor reopens the old files.
func TestDisableFileLogging_ForgetsTheFiles(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agency.log")
	if err := EnableFileLogging(logPath, false); err != nil {
		t.Fatal(err)
	}
	DisableFileLogging()

	if got := GetLogFilePath(); got != "" {
		t.Errorf("GetLogFilePath after disable = %q, want empty", got)
	}
	for _, name := range []string{"agency.log", errorLogName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RollLogFile(); err != nil {
		t.Fatalf("RollLogFile after disable = %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("files after a roll with logging disabled: %v, %v", entries, err)
	}
}
