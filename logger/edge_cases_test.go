// Agency
// License: MIT

package logger

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// messages returns the message of each captured line.
func messages(lines []map[string]any) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, str(l["message"]))
	}
	return out
}

func stubExit(t *testing.T) *[]int {
	t.Helper()
	var codes []int
	prev := osExit
	osExit = func(code int) { codes = append(codes, code) }
	t.Cleanup(func() { osExit = prev })
	return &codes
}

func TestPackageHelpers_LevelsAndFields(t *testing.T) {
	codes := stubExit(t)
	lines := captureSDK(t, DEBUG, func() {
		DebugF("d1", map[string]any{"k": "v"})
		DebugCF("comp", "d2", map[string]any{"n": 1})
		ErrorC("comp", "e1")
		ErrorCF("comp", "e2", map[string]any{"ok": true})
		Fatal("f1")
		FatalC("comp", "f2")
		Fatalf("f%d", 3)
		FatalF("f4", map[string]any{"x": 1.5})
	})
	want := []string{"d1", "d2", "e1", "e2", "f1", "f2", "f3", "f4"}
	if got := messages(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	levels := []string{"debug", "debug", "error", "error", "fatal", "fatal", "fatal", "fatal"}
	for i, l := range lines {
		if l["level"] != levels[i] {
			t.Errorf("line %d level = %v, want %s", i, l["level"], levels[i])
		}
	}
	if lines[0]["k"] != "v" || lines[3]["ok"] != true {
		t.Errorf("fields not logged: %v / %v", lines[0], lines[3])
	}
	if len(*codes) != 4 {
		t.Errorf("exit called %d times, want once per fatal line (4)", len(*codes))
	}
}

func TestLoggerType_Methods(t *testing.T) {
	codes := stubExit(t)
	SetLogMessageContent(false)
	t.Cleanup(func() { SetLogMessageContent(false) })

	l := NewLogger("alice")
	lines := captureSDK(t, DEBUG, func() {
		l.Debug("dbg")
		l.Debugf("dbg%d", 2)
		l.Info("inf")
		l.Infof("inf%d", 2)
		l.Warn("wrn")
		l.Warnf("wrn%d", 2)
		l.Warningf("wrn%d", 3)
		l.Error("err")
		l.Errorf("err%d", 2)
		l.Fatalf("fat%d", 1)
	})
	want := "dbg,dbg2,inf,inf2,wrn,wrn2,wrn3,err,err2,fat1"
	if got := strings.Join(messages(lines), ","); got != want {
		t.Fatalf("messages = %s, want %s", got, want)
	}
	if !strings.HasPrefix(str(lines[0]["caller"]), "alice") {
		t.Errorf("caller = %v, want the component first", lines[0]["caller"])
	}
	if len(*codes) != 1 {
		t.Errorf("exit calls = %d, want 1", len(*codes))
	}
	if err := l.Sync(); err != nil {
		t.Errorf("Sync = %v", err)
	}
}

func TestLoggerType_ContentSensitiveDebug(t *testing.T) {
	SetLogMessageContent(false)
	t.Cleanup(func() { SetLogMessageContent(false) })
	l := NewLogger("api").WithContentSensitive()

	lines := captureSDK(t, DEBUG, func() {
		l.Debug("body")
		l.Debugf("body %d", 1)
		l.Info("kept")
	})
	if got := strings.Join(messages(lines), ","); got != "kept" {
		t.Fatalf("with content logging off = %s, want only kept", got)
	}

	SetLogMessageContent(true)
	if !GetLogMessageContent() {
		t.Fatal("GetLogMessageContent = false after enabling")
	}
	lines = captureSDK(t, DEBUG, func() {
		l.Debug("body")
		l.Debugf("body %d", 1)
	})
	if got := strings.Join(messages(lines), ","); got != "body,body 1" {
		t.Fatalf("with content logging on = %s", got)
	}
}

func TestLoggerType_LogMapsLevels(t *testing.T) {
	l := NewLogger("discord")
	lines := captureSDK(t, DEBUG, func() {
		l.Log(int(WARN), 0, "plain %s", "warn")
		l.WithLevels(map[int]LogLevel{0: ERROR}).Log(0, 0, "mapped")
		l.Log(int(INFO), 0, "unmapped")
	})
	got := make([]string, 0, len(lines))
	for _, line := range lines {
		got = append(got, str(line["level"])+":"+str(line["message"]))
	}
	want := "warn:plain warn,error:mapped,info:unmapped"
	if strings.Join(got, ",") != want {
		t.Fatalf("lines = %v, want %s", got, want)
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]LogLevel{
		"debug": DEBUG, "\tDEBUG\n": DEBUG, "warn": WARN, "warning": WARN,
		"error": ERROR, "fatal": FATAL, "info": INFO, "": INFO, "loud": INFO,
	}
	for in, want := range tests {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDisableConsole(t *testing.T) {
	var buf bytes.Buffer
	restore := RedirectForTest(&buf)
	defer restore()
	DisableConsole()
	Info("silenced")
	if buf.Len() != 0 {
		t.Fatalf("console wrote %q after DisableConsole", buf.String())
	}
}

func TestGetLogFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agency.log")
	if err := EnableFileLogging(path, true); err != nil {
		t.Fatal(err)
	}
	defer DisableFileLogging()
	if got := GetLogFilePath(); got != path {
		t.Errorf("GetLogFilePath = %q, want %q", got, path)
	}
	// JSON file format carries the component as a field.
	SetLevel(INFO)
	InfoC("alice", "json line")
	data := mustReadFile(t, path)
	if !strings.Contains(string(data), `"component":"alice"`) || !strings.Contains(string(data), "json line") {
		t.Errorf("JSON log = %q", data)
	}
}

func TestEnableFileLogging_Errors(t *testing.T) {
	if os.Geteuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(ro, 0o700); err != nil {
				t.Error(err)
			}
		})
		if err := EnableFileLogging(filepath.Join(ro, "logs", "agency.log"), false); err == nil || !strings.Contains(err.Error(), "failed to create log directory") {
			t.Errorf("log dir under a read-only dir: %v", err)
		}
	}

	// agency.log is a directory.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "agency.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnableFileLogging(filepath.Join(dir, "agency.log"), false); err == nil || !strings.Contains(err.Error(), "failed to open log file") {
		t.Errorf("agency.log is a dir: %v", err)
	}

	// error.log is a directory: agency.log opens, error.log fails.
	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, errorLogName), 0o700); err != nil {
		t.Fatal(err)
	}
	err := EnableFileLogging(filepath.Join(dir2, "agency.log"), false)
	defer DisableFileLogging()
	if err == nil || !strings.Contains(err.Error(), "failed to open error log file") {
		t.Errorf("error.log is a dir: %v", err)
	}
}

// Re-enabling file logging replaces both open files.
func TestEnableFileLogging_Reopen(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "a", "agency.log"), filepath.Join(dir, "b", "agency.log")
	if err := EnableFileLogging(first, false); err != nil {
		t.Fatal(err)
	}
	defer DisableFileLogging()
	if err := EnableFileLogging(second, false); err != nil {
		t.Fatal(err)
	}
	SetLevel(INFO)
	SetErrorLogLevel(WARN)
	WarnC("test", "to second")
	if strings.Contains(string(mustReadFile(t, first)), "to second") {
		t.Error("line went to the replaced agency.log")
	}
	if !strings.Contains(string(mustReadFile(t, second)), "to second") {
		t.Error("line missing from the new agency.log")
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(dir, "b", errorLogName))), "to second") {
		t.Error("line missing from the new error.log")
	}
}

// A second roll on the same day appends to the dated archive.
func TestRollLogFile_SameDayAppends(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agency.log")
	if err := EnableFileLogging(logPath, false); err != nil {
		t.Fatal(err)
	}
	defer DisableFileLogging()
	SetLevel(INFO)
	ts := time.Date(2026, 3, 4, 10, 0, 0, 0, time.Local) //nolint:gosmopolitan // logs roll on the local calendar day by design

	for _, msg := range []string{"first roll", "second roll"} {
		InfoC("test", msg)
		if err := os.Chtimes(logPath, ts, ts); err != nil {
			t.Fatal(err)
		}
		if err := RollLogFile(); err != nil {
			t.Fatalf("RollLogFile: %v", err)
		}
	}
	data := string(mustReadFile(t, filepath.Join(dir, "20260304-agency.log")))
	if !strings.Contains(data, "first roll") || !strings.Contains(data, "second roll") {
		t.Fatalf("archive = %q, want both rolls", data)
	}
	// An empty error.log is not archived.
	if _, err := os.Stat(filepath.Join(dir, "20260304-error.log")); !os.IsNotExist(err) {
		t.Errorf("empty error.log archived: %v", err)
	}
}

func TestArchiveLogByMtime_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := archiveLogByMtime(filepath.Join(dir, "missing.log")); err != nil {
		t.Errorf("missing file: %v", err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveLogByMtime(filepath.Join(blocker, "x.log")); err == nil {
		t.Error("stat through a file: want error")
	}
}

func TestAppendAndRemove_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := appendAndRemove(filepath.Join(dir, "missing"), filepath.Join(dir, "dst")); err == nil {
		t.Error("missing src: want error")
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendAndRemove(src, filepath.Join(dir, "missing-dst")); err == nil {
		t.Error("missing dst: want error")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("src removed after a failed append: %v", err)
	}
}

func TestSDKHandler_EmptyAttrsAndGroups(t *testing.T) {
	// No attributes and an empty group name change nothing: fields stay
	// ungrouped.
	h := SDK().Handler().WithAttrs(nil).WithGroup("")
	lines := captureSDK(t, DEBUG, func() {
		slog.New(h).LogAttrs(t.Context(), slog.LevelInfo, "m",
			slog.Attr{},                           // empty: ignored
			slog.Group("g"),                       // empty group: skipped
			slog.Group("", slog.Int("inline", 1)), // keyless group: inlined
		)
	})
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	if _, ok := lines[0]["g"]; ok {
		t.Error("empty group produced a field")
	}
	if lines[0]["inline"] != float64(1) {
		t.Errorf("inline group field = %v", lines[0]["inline"])
	}
	if _, ok := lines[0][""]; ok {
		t.Error("empty attribute produced a field")
	}
}

// str is v as a string, or "" when it is not one.
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
