// ClawEh
// License: MIT

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.bison9.com/agency/config"
)

// GET /api/agents/allowed-folders/ignored names each saved allowed folder the
// file tools skip, with why, so the Agents page can mark it; a usable folder
// is not listed.
func TestHandleIgnoredAllowedFolders(t *testing.T) {
	configPath := setupTestEnv(t)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "alice", AllowedFolders: []config.AllowedFolderConfig{
		{Path: t.TempDir()}, {Path: "notes"}, {Path: missing},
	}})
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/allowed-folders/ignored", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ignored []ignoredAllowedFolder `json:"ignored"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []ignoredAllowedFolder{
		{Agent: "alice", Path: "notes", Reason: config.FolderNotAbsolute},
		{Agent: "alice", Path: missing, Reason: config.FolderNotFound},
	}
	if len(body.Ignored) != 2 || body.Ignored[0] != want[0] || body.Ignored[1] != want[1] {
		t.Errorf("ignored = %+v, want %+v", body.Ignored, want)
	}
}

// GET /api/agents/knowledge/missing names each saved shared knowledge base
// whose folder is missing, so the Agents page can mark its row; one whose
// folder exists is not listed.
func TestHandleMissingSharedKnowledge(t *testing.T) {
	configPath := setupTestEnv(t)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "alice", Knowledge: &config.KnowledgeConfig{
		Shared: []config.SharedKnowledgeConfig{{Name: "here", Path: t.TempDir()}, {Name: "gone", Path: missing}},
	}})
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/knowledge/missing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Missing []missingSharedKnowledge `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := missingSharedKnowledge{Agent: "alice", Name: "gone", Path: missing}
	if len(body.Missing) != 1 || body.Missing[0] != want {
		t.Errorf("missing = %+v, want [%+v]", body.Missing, want)
	}
}
