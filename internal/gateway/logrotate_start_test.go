// ClawEh
// License: MIT

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.bison9.com/agency/logger"
)

// staleLog enables file logging to dir/claw.log, writes a line and dates the
// file yesterday. It returns the log path and yesterday's archive name.
func staleLog(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "claw.log")
	if err := logger.EnableFileLogging(logPath, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.DisableFileLogging)
	logger.WarnC("test", "a line from yesterday")
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := os.Chtimes(logPath, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	return logPath, filepath.Join(dir, yesterday.Format("20060102")+"-claw.log")
}

// TestStartLogRotation_RollsStaleLog: a claw.log last written before today is
// archived under its date at startup.
func TestStartLogRotation_RollsStaleLog(t *testing.T) {
	logPath, archived := staleLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &alertRecorder{}
	startLogRotation(ctx, logPath, 30, rec)
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("stale log not archived: %v", err)
	}
	if len(rec.titles()) != 0 {
		t.Errorf("alerts = %v", rec.titles())
	}
}

// TestStartLogRotation_FailureAlerts: a roll that cannot archive the log
// alerts.
func TestStartLogRotation_FailureAlerts(t *testing.T) {
	logPath, archived := staleLog(t)
	if err := os.Mkdir(archived, 0o700); err != nil { // the archive name is taken by a folder
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &alertRecorder{}
	startLogRotation(ctx, logPath, 0, rec)
	if !rec.has("Log rotation failed") {
		t.Fatalf("alerts = %v", rec.titles())
	}
}
