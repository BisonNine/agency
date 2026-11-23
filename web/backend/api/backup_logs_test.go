// ClawEh
// License: MIT

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/logger"
)

// POST /api/backup writes an archive into the configured backup folder and
// reports where; a folder that cannot be created is a 500 naming the failure.
func TestRunBackup(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	cfg, err := h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	rec := serve(mux, http.MethodPost, "/api/backup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Folder  string   `json:"folder"`
		Archive string   `json:"archive"`
		Bytes   int64    `json:"bytes"`
		Files   int      `json:"files"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	wantFolder := filepath.Join(cfg.DataDir(), "backup")
	if out.Folder != wantFolder || filepath.Dir(out.Archive) != wantFolder {
		t.Fatalf("folder %q archive %q, want both in %q", out.Folder, out.Archive, wantFolder)
	}
	fi, err := os.Stat(out.Archive)
	if err != nil || fi.Size() != out.Bytes || out.Files == 0 || out.Skipped == nil {
		t.Fatalf("archive stat %v err %v; reported bytes %d files %d skipped %v", fi, err, out.Bytes, out.Files, out.Skipped)
	}

	// A file where the backup folder belongs makes the backup fail.
	if err := os.RemoveAll(wantFolder); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantFolder, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec = serve(mux, http.MethodPost, "/api/backup", "")
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Body.String(), "backup failed: ") {
		t.Fatalf("failing backup: %d %q", rec.Code, rec.Body.String())
	}
}

// GET /api/gateway/logs tails claw.log (lines bounded and defaulted), never
// cached; with file logging off, or the file gone, it says why instead.
func TestGatewayLogs(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	get := func(query string) (map[string]any, *http.Response) {
		t.Helper()
		rec := serve(mux, http.MethodGet, "/api/gateway/logs"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		return decodeMap(t, rec), rec.Result()
	}

	// Only a process that never enabled file logging has no log path:
	// logger.DisableFileLogging keeps the last one (a logger defect,
	// reported separately), so a repeated run starts with one set.
	if logger.GetLogFilePath() == "" {
		out, resp := get("")
		if out["error"] != "file logging is disabled" || out["count"] != float64(0) {
			t.Fatalf("disabled: %v", out)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
		}
	}

	logPath := filepath.Join(t.TempDir(), "logs", "claw.log")
	if err := logger.EnableFileLogging(logPath, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.DisableFileLogging)
	for _, m := range []string{"alice-one", "alice-two", "alice-three"} {
		logger.WarnCF("test", m, nil)
	}

	out, resp := get("?lines=1")
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	logs, _ := out["logs"].([]any)
	if out["count"] != float64(1) || len(logs) != 1 || !strings.Contains(logs[0].(string), "alice-three") {
		t.Fatalf("lines=1: %v", out)
	}
	for _, q := range []string{"", "?lines=abc", "?lines=-3", "?lines=999999"} {
		out, _ = get(q)
		if n, _ := out["count"].(float64); n < 3 || out["error"] != nil {
			t.Fatalf("%q: count %v error %v, want every line", q, out["count"], out["error"])
		}
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	out, _ = get("")
	if e, _ := out["error"].(string); !strings.Contains(e, "no such file") || out["count"] != float64(0) {
		t.Fatalf("missing file: %v", out)
	}
}
