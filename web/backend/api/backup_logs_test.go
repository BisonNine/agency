// Agency
// License: MIT

package api

import (
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
	if err = os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
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
	decodeInto(t, rec, &out)
	wantFolder := filepath.Join(cfg.DataDir(), "backup")
	if out.Folder != wantFolder || filepath.Dir(out.Archive) != wantFolder {
		t.Fatalf("folder %q archive %q, want both in %q", out.Folder, out.Archive, wantFolder)
	}
	fi, err := os.Stat(out.Archive)
	if err != nil || fi.Size() != out.Bytes || out.Files == 0 || out.Skipped == nil {
		t.Fatalf("archive stat %v err %v; reported bytes %d files %d skipped %v", fi, err, out.Bytes, out.Files, out.Skipped)
	}

	// A file where the backup folder belongs makes the backup fail.
	if err = os.RemoveAll(wantFolder); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(wantFolder, nil, 0o600); err != nil {
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
	type logsReply struct {
		Logs  []string `json:"logs"`
		Count int      `json:"count"`
		Error string   `json:"error"`
	}
	get := func(query string) logsReply {
		t.Helper()
		rec := serve(mux, http.MethodGet, "/api/gateway/logs"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", cc)
		}
		var out logsReply
		decodeInto(t, rec, &out)
		return out
	}

	// Only a process that never enabled file logging has no log path:
	// logger.DisableFileLogging keeps the last one (a logger defect,
	// reported separately), so a repeated run starts with one set.
	if logger.GetLogFilePath() == "" {
		if out := get(""); out.Error != "file logging is disabled" || out.Count != 0 || out.Logs == nil {
			t.Fatalf("disabled: %+v", out)
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

	out := get("?lines=1")
	if out.Count != 1 || len(out.Logs) != 1 || !strings.Contains(out.Logs[0], "alice-three") {
		t.Fatalf("lines=1: %+v", out)
	}
	for _, q := range []string{"", "?lines=abc", "?lines=-3", "?lines=999999"} {
		if out = get(q); out.Count < 3 || out.Error != "" {
			t.Fatalf("%q: %+v, want every line", q, out)
		}
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if out = get(""); !strings.Contains(out.Error, "no such file") || out.Count != 0 {
		t.Fatalf("missing file: %+v", out)
	}
}
