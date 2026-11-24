package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRollLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agency.log")

	if err := EnableFileLogging(logPath, false); err != nil {
		t.Fatalf("EnableFileLogging: %v", err)
	}
	defer DisableFileLogging()
	SetLevel(DEBUG)
	SetErrorLogLevel(WARN)

	InfoCF("test", "an info line", nil)
	WarnCF("test", "a warning line", nil)

	// Force a known mtime so the archive name is deterministic.
	ts := time.Date(2026, 1, 2, 10, 0, 0, 0, time.Local) //nolint:gosmopolitan // logs roll on the local calendar day by design
	if err := os.Chtimes(logPath, ts, ts); err != nil {
		t.Fatalf("chtimes agency.log: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "error.log"), ts, ts); err != nil {
		t.Fatalf("chtimes error.log: %v", err)
	}

	if err := RollLogFile(); err != nil {
		t.Fatalf("RollLogFile: %v", err)
	}

	// Both active logs are archived under the mtime date.
	agencyArchive := filepath.Join(dir, "20260102-agency.log")
	errArchive := filepath.Join(dir, "20260102-error.log")
	if _, err := os.Stat(agencyArchive); err != nil {
		t.Fatalf("expected dated agency archive: %v", err)
	}
	if _, err := os.Stat(errArchive); err != nil {
		t.Fatalf("expected dated error archive: %v", err)
	}

	// The warning is in both agency and error archives; the info only in agency.
	agencyData := mustReadFile(t, agencyArchive)
	errData := mustReadFile(t, errArchive)
	if !strings.Contains(string(agencyData), "an info line") {
		t.Fatalf("agency archive missing info line: %q", agencyData)
	}
	if !strings.Contains(string(errData), "a warning line") {
		t.Fatalf("error archive missing warning line: %q", errData)
	}
	if strings.Contains(string(errData), "an info line") {
		t.Fatalf("error.log should not contain info-level lines: %q", errData)
	}

	// Fresh active files are reopened and receive new writes.
	WarnCF("test", "after the roll", nil)
	freshErr := mustReadFile(t, filepath.Join(dir, "error.log"))
	if !strings.Contains(string(freshErr), "after the roll") {
		t.Fatalf("reopened error.log missing post-roll line: %q", freshErr)
	}
	if strings.Contains(string(freshErr), "a warning line") {
		t.Fatalf("reopened error.log should not retain pre-roll content: %q", freshErr)
	}
}

func TestRollLogFile_NoFileLogging(t *testing.T) {
	DisableFileLogging()
	if err := RollLogFile(); err != nil {
		t.Fatalf("RollLogFile with no file logging should be a no-op, got %v", err)
	}
}

func TestFatalWritesToAllSinksBeforeExit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agency.log")
	if err := EnableFileLogging(logPath, false); err != nil {
		t.Fatalf("EnableFileLogging: %v", err)
	}
	defer DisableFileLogging()
	SetLevel(DEBUG)
	SetErrorLogLevel(WARN)

	// Stub the exit so the process survives and we can inspect the sinks.
	exitCode := -1
	prev := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = prev }()

	FatalCF("test", "fatal boom", nil)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	agencyData := mustReadFile(t, logPath)
	errData := mustReadFile(t, filepath.Join(dir, "error.log"))
	if !strings.Contains(string(agencyData), "fatal boom") {
		t.Fatalf("agency.log missing fatal line: %q", agencyData)
	}
	if !strings.Contains(string(errData), "fatal boom") {
		t.Fatalf("error.log missing fatal line: %q", errData)
	}
}

// mustReadFile reads path or fails the test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
