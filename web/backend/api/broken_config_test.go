// Agency
// License: MIT

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When config.json cannot be read every endpoint that needs it fails with a
// 5xx naming the failure and saves nothing; none answers as if it had worked.
func TestHandlers_BrokenConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENCY_HOME", filepath.Join(dir, ".agency"))
	p := filepath.Join(dir, "config.json")
	const broken = "{not json"
	if err := os.WriteFile(p, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	const (
		loadFailed = "Failed to load config: invalid character"
		saveFailed = `{"errors":["Failed to save config: invalid character`
	)
	for _, tc := range []struct {
		method, path string
		status       int
		prefix       string
	}{
		{"GET", "/api/agents/human", 500, "invalid character"},
		{"GET", "/api/agents/allowed-folders/ignored", 500, "invalid character"},
		{"GET", "/api/agents/knowledge/missing", 500, "invalid character"},
		{"GET", "/api/config", 500, loadFailed},
		{"PUT", "/api/config", 500, saveFailed},
		{"PATCH", "/api/config", 500, loadFailed},
		{"POST", "/api/backup", 500, "failed to load config\n"},
		{"POST", "/api/devices/pair", 500, saveFailed},
		{"GET", "/api/devices/pair", 500, `{"error":"config load failed"}`},
		{"POST", "/api/devices/settings", 500, saveFailed},
		{"POST", "/api/devices/word-token/regenerate", 500, saveFailed},
		{"GET", "/api/devices/pending", 500, `{"error":"store open failed"}`},
		{"GET", "/api/devices", 500, `{"error":"store open failed"}`},
		{"GET", "/api/cards", 500, `{"error":"load config: invalid character`},
		{"GET", "/api/cards/x", 500, `{"error":"load config: invalid character`},
		{"POST", "/api/cards/import", 500, `{"error":"load config: invalid character`},
		{"DELETE", "/api/cards/x", 500, `{"error":"load config: invalid character`},
		{"GET", "/api/memory", 500, "failed to resolve sessions directory\n"},
		{"GET", "/api/report/pdf", 500, loadFailed},
		{"GET", "/api/report/assessment", 500, `{"error":"` + loadFailed},
		{"GET", "/api/system/clis/registration", 500, `{"error":"` + loadFailed},
		{"POST", "/api/system/clis/antigravity-cli/register", 500, `{"error":"` + loadFailed},
		{"GET", "/api/system/clis", 500, loadFailed},
		{"GET", "/api/skills", 500, loadFailed},
		{"GET", "/api/skills/x", 500, loadFailed},
		{"POST", "/api/skills/import", 500, loadFailed},
		{"DELETE", "/api/skills/x", 500, loadFailed},
		{"GET", "/api/task-models", 500, loadFailed},
		{"PUT", "/api/task-models", 500, saveFailed},
		{"GET", "/api/agents/main/message-tokens", 500, `{"error":"config load failed"}`},
		{"POST", "/api/agents/main/message-tokens", 500, `{"error":"config load failed"}`},
		{"PATCH", "/api/agents/main/message-tokens/t", 500, `{"error":"config load failed"}`},
		{"DELETE", "/api/agents/main/message-tokens/t", 500, `{"error":"config load failed"}`},
		{"GET", "/api/channels/secmsg/x/accounts", 500, `{"error":"failed to load config"}`},
		{"GET", "/api/system/setup-status", 500, loadFailed},
		{"GET", "/api/providers", 500, loadFailed},
		{"POST", "/api/providers", 500, saveFailed},
		{"PUT", "/api/providers/0", 500, saveFailed},
		{"DELETE", "/api/providers/0", 500, saveFailed},
		{"POST", "/api/webui/setup", 500, saveFailed},
		{"GET", "/api/tls", 500, `{"error":"failed to load config: invalid character`},
		{"GET", "/api/models", 500, loadFailed},
		{"PUT", "/api/models/0", 500, saveFailed},
		{"DELETE", "/api/models/0", 500, saveFailed},
	} {
		rec := serve(mux, tc.method, tc.path, "{}")
		if rec.Code != tc.status || !strings.HasPrefix(rec.Body.String(), tc.prefix) {
			t.Errorf("%s %s = %d %q, want %d starting %q", tc.method, tc.path, rec.Code, rec.Body.String(), tc.status, tc.prefix)
		}
	}
	if raw, err := os.ReadFile(p); err != nil || string(raw) != broken {
		t.Fatalf("config.json was changed: %q %v", raw, err)
	}
}
