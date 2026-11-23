// ClawEh
// License: MIT

package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cogmemstore "go.bison9.com/agent/cogmem/store"

	"go.bison9.com/agency/utils"
)

// memoryDBPath returns the seeded store's database file.
func memoryDBPath(t *testing.T, configPath string) string {
	t.Helper()
	return memoryDBForSessionsDir(sessionsTestDir(t, configPath))
}

// The detail reports a touched domain's last use and the last consolidation
// run as stored.
func TestHandleGetMemoryStore_LastUsedAndLastRun(t *testing.T) {
	configPath := setupTestEnv(t)
	id := seedCogmemDB(t, configPath)
	s, err := cogmemstore.Open(memoryDBPath(t, configPath))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	domains, err := s.ListDomains(ctx, s.DB(), cogmemstore.StatusActive)
	if err != nil || len(domains) == 0 {
		t.Fatalf("domains %v %v", domains, err)
	}
	var seeded cogmemstore.Domain
	for _, d := range domains {
		if d.Name == "Website Redesign" {
			seeded = d
		}
	}
	if err := s.Touch(ctx, s.DB(), seeded.ID); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	if err := s.RecordRun(ctx, s.DB(), cogmemstore.Run{ID: "r1", Trigger: "nightly", Status: "ok", OpsApplied: 4, Note: "repaired", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	utils.CloseQuietly(s)

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := serve(mux, http.MethodGet, "/api/memory/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var detail memoryDetailResponse
	decodeInto(t, rec, &detail)
	run := detail.LastRun
	if run == nil || run.Trigger != "nightly" || run.Status != "ok" || run.OpsApplied != 4 || run.Note != "repaired" {
		t.Fatalf("last_run = %+v", run)
	}
	if at, err := time.Parse(time.RFC3339, run.StartedAt); err != nil || !at.Equal(started) {
		t.Fatalf("started_at = %q, want %v", run.StartedAt, started)
	}
	for _, d := range detail.Domains {
		if d.ID != seeded.ID {
			continue
		}
		if _, err := time.Parse(time.RFC3339, d.LastUsed); err != nil {
			t.Fatalf("last_used = %q, want an RFC3339 time", d.LastUsed)
		}
		return
	}
	t.Fatal("seeded domain missing from the detail")
}

// A store id that could name a path is refused before any lookup, on every
// memory endpoint.
func TestMemoryEndpoints_RefuseTraversalID(t *testing.T) {
	configPath := setupTestEnv(t)
	seedCogmemDB(t, configPath)
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/memory/a..b"},
		{http.MethodGet, "/api/memory/a..b/export"},
		{http.MethodDelete, "/api/memory/a..b/domains/d"},
		{http.MethodDelete, "/api/memory/a..b/memories/m"},
		{http.MethodPost, "/api/memory/a..b/domains"},
		{http.MethodPost, "/api/memory/a..b/import"},
		{http.MethodPatch, "/api/memory/a..b/memories/m"},
		{http.MethodPost, "/api/memory/a..b/domains/d/memories"},
		{http.MethodPost, "/api/memory/a..b/bulk"},
		{http.MethodGet, `/api/memory/a%5Cb`},
	} {
		rec := serve(mux, tc.method, tc.path, "")
		if rec.Code != http.StatusBadRequest || rec.Body.String() != "invalid memory id\n" {
			t.Errorf("%s %s = %d %q, want 400 invalid memory id", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/memory/nobody/export"},
		{http.MethodDelete, "/api/memory/nobody/domains/d"},
	} {
		rec := serve(mux, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound || rec.Body.String() != "memory store not found\n" {
			t.Errorf("%s %s = %d %q, want 404", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// A database file that is not a database fails each endpoint with its own
// 500, without detail.
func TestMemoryEndpoints_CorruptStore(t *testing.T) {
	configPath := setupTestEnv(t)
	id := seedCogmemDB(t, configPath)
	path := memoryDBPath(t, configPath)
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database ", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/memory/" + id},
		{http.MethodGet, "/api/memory/" + id + "/export"},
		{http.MethodDelete, "/api/memory/" + id + "/domains/d"},
		{http.MethodPost, "/api/memory/" + id + "/import"},
	} {
		rec := serve(mux, tc.method, tc.path, "")
		if rec.Code != http.StatusInternalServerError || rec.Body.String() != "failed to open memory store\n" {
			t.Errorf("%s %s = %d %q, want 500 failed to open memory store", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// Deleting a domain that does not exist is a 404.
func TestDeleteDomain_NotFound(t *testing.T) {
	id, mux, _ := curateEnv(t)
	rec := serve(mux, http.MethodDelete, "/api/memory/"+id+"/domains/missing", "")
	if rec.Code != http.StatusNotFound || rec.Body.String() != "domain not found\n" {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
}

// Creating a domain refuses a body that is not JSON, a blank name and a name
// already taken, and stores the trimmed name otherwise.
func TestCreateDomain_Refusals(t *testing.T) {
	id, mux, _ := curateEnv(t)
	url := "/api/memory/" + id + "/domains"
	for _, tc := range []struct {
		body   string
		status int
		want   string
	}{
		{"{", http.StatusBadRequest, "invalid request body\n"},
		{`{"name":"   "}`, http.StatusBadRequest, "name is required\n"},
	} {
		rec := serve(mux, http.MethodPost, url, tc.body)
		if rec.Code != tc.status || rec.Body.String() != tc.want {
			t.Errorf("%s = %d %q, want %d %q", tc.body, rec.Code, rec.Body.String(), tc.status, tc.want)
		}
	}
	rec := serve(mux, http.MethodPost, url, `{"name":"  Garden  ","summary":"plants"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	var d memoryDomain
	decodeInto(t, rec, &d)
	if d.Name != "Garden" || d.Summary != "plants" || d.Status != "active" {
		t.Fatalf("created = %+v", d)
	}
	rec = serve(mux, http.MethodPost, url, `{"name":"Garden"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d %q, want 409", rec.Code, rec.Body.String())
	}
}

// Creating a memory refuses a body that is not JSON and an unknown type, and
// a domain that does not exist is a 404.
func TestCreateMemory_Refusals(t *testing.T) {
	id, mux, domain := curateEnv(t)
	base := "/api/memory/" + id + "/domains/"
	for _, tc := range []struct {
		domain, body string
		status       int
		want         string
	}{
		{domain, "{", http.StatusBadRequest, "invalid request body\n"},
		{domain, `{"type":"nonsense","text":"x"}`, http.StatusBadRequest, "unknown memory type\n"},
		{"missing", `{"type":"fact","text":"x"}`, http.StatusNotFound, "memory not found\n"},
	} {
		rec := serve(mux, http.MethodPost, base+tc.domain+"/memories", tc.body)
		if rec.Code != tc.status || rec.Body.String() != tc.want {
			t.Errorf("%s %s = %d %q, want %d %q", tc.domain, tc.body, rec.Code, rec.Body.String(), tc.status, tc.want)
		}
	}
}

// The export is a YAML attachment named after the store and the day.
func TestExportMemory_Headers(t *testing.T) {
	id, mux, _ := curateEnv(t)
	rec := serve(mux, http.MethodGet, "/api/memory/"+id+"/export", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `attachment; filename="` + id + `-memory-` + time.Now().UTC().Format("20060102") + `.yaml"`
	if cd := rec.Header().Get("Content-Disposition"); cd != want {
		t.Fatalf("Content-Disposition = %q, want %q", cd, want)
	}
	if !strings.Contains(rec.Body.String(), "drove to the KOA") {
		t.Fatalf("export lacks the stored memory: %s", rec.Body.String())
	}
}

// Bulk restore brings retired memories back; a body that is not JSON is
// refused.
func TestBulkRestoreAndBadBody(t *testing.T) {
	id, mux, domain := curateEnv(t)
	mems := memoriesOf(t, mux, id, domain, false)
	ids := []string{mems[0].ID, mems[1].ID}
	url := "/api/memory/" + id + "/bulk"
	if rec := do(t, mux, http.MethodPost, url, map[string]any{"action": "retire", "ids": ids}); rec.Code != http.StatusOK {
		t.Fatalf("retire = %d %q", rec.Code, rec.Body.String())
	}
	if n := len(memoriesOf(t, mux, id, domain, false)); n != 1 {
		t.Fatalf("%d active after retire, want 1", n)
	}
	rec := do(t, mux, http.MethodPost, url, map[string]any{"action": "restore", "ids": ids})
	if rec.Code != http.StatusOK || rec.Body.String() != `{"changed":2}`+"\n" {
		t.Fatalf("restore = %d %q", rec.Code, rec.Body.String())
	}
	if n := len(memoriesOf(t, mux, id, domain, false)); n != 3 {
		t.Fatalf("%d active after restore, want 3", n)
	}
	rec = serve(mux, http.MethodPost, url, "{")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "invalid request body\n" {
		t.Fatalf("bad body = %d %q", rec.Code, rec.Body.String())
	}
}
