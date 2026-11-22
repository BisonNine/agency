// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureSDK runs fn with the console redirected and returns each line it
// logged, decoded.
func captureSDK(t *testing.T, level LogLevel, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	restore := RedirectForTest(&buf)
	defer restore()
	SetLevel(level)
	fn()
	var lines []map[string]any
	for raw := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, raw)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestSDKLogger_Levels(t *testing.T) {
	tests := []struct {
		name  string
		level slog.Level
		want  string
	}{
		{"debug", slog.LevelDebug, "debug"},
		{"below debug", slog.LevelDebug - 4, "debug"},
		{"info", slog.LevelInfo, "info"},
		{"between info and warn", slog.LevelInfo + 2, "info"},
		{"warn", slog.LevelWarn, "warn"},
		{"error", slog.LevelError, "error"},
		{"above error", slog.LevelError + 4, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := captureSDK(t, DEBUG, func() {
				SDK().Log(t.Context(), tt.level, "hello", "component", "spawnllm")
			})
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			if got := lines[0]["level"]; got != tt.want {
				t.Errorf("level = %v, want %s", got, tt.want)
			}
			if got := lines[0]["message"]; got != "hello" {
				t.Errorf("message = %v, want hello", got)
			}
		})
	}
}

func TestSDKLogger_HostLevelFilters(t *testing.T) {
	lines := captureSDK(t, WARN, func() {
		if SDK().Enabled(t.Context(), slog.LevelInfo) {
			t.Error("info is enabled at host level warn")
		}
		if !SDK().Enabled(t.Context(), slog.LevelWarn) {
			t.Error("warn is not enabled at host level warn")
		}
		SDK().Info("dropped", "component", "cogmem")
		SDK().Warn("kept", "component", "cogmem")
	})
	if len(lines) != 1 || lines[0]["message"] != "kept" {
		t.Fatalf("lines = %v, want only the warn line", lines)
	}
}

// TestSDKLogger_Component: the component attribute is the line's component,
// as the former per-package backends passed it, and not repeated as a field.
func TestSDKLogger_Component(t *testing.T) {
	tests := []struct {
		name string
		log  func(*slog.Logger)
		want string
	}{
		{"record attribute", func(l *slog.Logger) { l.Info("m", "component", "llmcontext") }, "llmcontext"},
		{"WithAttrs", func(l *slog.Logger) { l.With("component", "memory").Info("m") }, "memory"},
		{"record overrides WithAttrs", func(l *slog.Logger) { l.With("component", "memory").Info("m", "component", "session") }, "session"},
		{"none", func(l *slog.Logger) { l.Info("m") }, ""},
		{"grouped component is a field", func(l *slog.Logger) { l.WithGroup("g").Info("m", "component", "x") }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := captureSDK(t, DEBUG, func() { tt.log(SDK()) })
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			caller := fmt.Sprint(lines[0]["caller"])
			if tt.want == "" {
				if !strings.HasPrefix(caller, "<none> ") {
					t.Errorf("caller = %q, want no component", caller)
				}
			} else if !strings.HasPrefix(caller, tt.want+" ") {
				t.Errorf("caller = %q, want component %q", caller, tt.want)
			}
			if _, ok := lines[0]["component"]; ok && tt.want != "" {
				t.Errorf("component repeated as a field: %v", lines[0])
			}
		})
	}
}

func TestSDKLogger_Fields(t *testing.T) {
	lines := captureSDK(t, DEBUG, func() {
		SDK().Info("m",
			"component", "spawnllm",
			"text", "abc",
			"count", 3,
			"ratio", 0.5,
			"ok", true,
			"took", 2*time.Second,
			"error", errors.New("boom"),
			"list", []string{"a", "b"},
		)
	})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	l := lines[0]
	checks := map[string]any{
		"text":  "abc",
		"count": float64(3),
		"ratio": 0.5,
		"ok":    true,
		"took":  float64(2 * time.Second), // as the former backends wrote a Duration
		"error": "boom",
	}
	for k, want := range checks {
		if l[k] != want {
			t.Errorf("%s = %#v, want %#v", k, l[k], want)
		}
	}
	if list, ok := l["list"].([]any); !ok || len(list) != 2 || list[0] != "a" {
		t.Errorf("list = %#v, want [a b]", l["list"])
	}
}

func TestSDKLogger_GroupsAndWithAttrs(t *testing.T) {
	lines := captureSDK(t, DEBUG, func() {
		l := SDK().With("component", "cogmem", "agent", "alice")
		l.WithGroup("req").With("id", 7).Info("m",
			"status", 200,
			slog.Group("usage", "in", 10, "out", 2),
			slog.Group("empty"),
			slog.Group("", "inline", "x"),
		)
		// A logger derived from l does not change l.
		l.Info("second")
	})
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	first := lines[0]
	want := map[string]any{
		"agent":         "alice",
		"req.id":        float64(7),
		"req.status":    float64(200),
		"req.usage.in":  float64(10),
		"req.usage.out": float64(2),
		"req.inline":    "x",
	}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("%s = %#v, want %#v (line %v)", k, first[k], v, first)
		}
	}
	for _, k := range []string{"req.empty", "empty", "id", "status"} {
		if _, ok := first[k]; ok {
			t.Errorf("unexpected field %q in %v", k, first)
		}
	}
	if caller := fmt.Sprint(first["caller"]); !strings.HasPrefix(caller, "cogmem ") {
		t.Errorf("caller = %q, want component cogmem", caller)
	}
	second := lines[1]
	if second["agent"] != "alice" {
		t.Errorf("second line lost the logger's attributes: %v", second)
	}
	if _, ok := second["req.id"]; ok {
		t.Errorf("second line has the derived logger's attributes: %v", second)
	}
}

// TestSDKLogger_ErrorLog: an SDK error reaches error.log like the host's own.
func TestSDKLogger_ErrorLog(t *testing.T) {
	dir := t.TempDir()
	if err := EnableFileLogging(filepath.Join(dir, "claw.log"), true); err != nil {
		t.Fatal(err)
	}
	defer DisableFileLogging()
	captureSDK(t, DEBUG, func() {
		SDK().Error("store failed", "component", "memory", "path", "/x")
	})
	for _, name := range []string{"claw.log", errorLogName} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var line map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(data), &line); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, data)
		}
		if line["component"] != "memory" || line["path"] != "/x" || line["message"] != "store failed" || line["level"] != "error" {
			t.Errorf("%s line = %v", name, line)
		}
	}
}

// TestSDKAs: SDKAs logs the SDK's component under the host's own name and
// leaves every other component as it is.
func TestSDKAs(t *testing.T) {
	lines := captureSDK(t, DEBUG, func() {
		l := SDKAs("tools", "session-tools")
		l.With("component", "tools").Info("renamed", "k", "v")
		l.Info("kept", "component", "memory")
	})
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, want := range []string{"session-tools ", "memory "} {
		if caller := fmt.Sprint(lines[i]["caller"]); !strings.HasPrefix(caller, want) {
			t.Errorf("line %d caller = %q, want component %q", i, caller, strings.TrimSpace(want))
		}
	}
	if lines[0]["k"] != "v" {
		t.Errorf("fields lost: %v", lines[0])
	}
}
