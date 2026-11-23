// ClawEh
// License: MIT

package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillsMux is a handler on the shared test config with every route.
func skillsMux(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, filepath.Join(os.Getenv("CLAW_HOME"), "skills")
}

// importSkill uploads content as filename to POST /api/skills/import.
func importSkill(t *testing.T, mux *http.ServeMux, field, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/skills/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The frontmatter name and description win over the filename; comments,
// blank lines and lines without a colon in it are ignored; CRLF line ends
// are normalised; the saved file carries only name and description.
func TestImportSkill_FrontmatterWins(t *testing.T) {
	mux, shared := skillsMux(t)
	content := "---\r\n# a comment\r\n\r\nname: \"Trip Planner\"\r\ndescription: 'Plans trips'\r\nno colon here\r\nauthor: Alice\r\n---\r\n\r\n# Heading\r\nBody line\r\n"
	rec := importSkill(t, mux, "file", "ignored.md", content)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"trip-planner"`) {
		t.Fatalf("response = %s", rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(shared, "trip-planner", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: trip-planner\ndescription: Plans trips\n---\n\n# Heading\nBody line\n"
	if string(got) != want {
		t.Fatalf("saved = %q, want %q", got, want)
	}

	rec = importSkill(t, mux, "file", "other.md", content)
	if rec.Code != http.StatusConflict || rec.Body.String() != "skill already exists\n" {
		t.Fatalf("duplicate = %d %q", rec.Code, rec.Body.String())
	}
}

// A file with no text gets the placeholder description; a long one is cut
// at 1024 characters.
func TestImportSkill_Descriptions(t *testing.T) {
	mux, shared := skillsMux(t)
	if rec := importSkill(t, mux, "file", "empty.md", "\n\n"); rec.Code != http.StatusOK {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(shared, "empty", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "---\nname: empty\ndescription: Imported skill\n---\n\n" {
		t.Fatalf("empty saved = %q", got)
	}

	long := strings.Repeat("x", 2000)
	if rec := importSkill(t, mux, "file", "long.md", "---\ndescription: "+long+"\n---\nbody"); rec.Code != http.StatusOK {
		t.Fatalf("long: %d %s", rec.Code, rec.Body.String())
	}
	got, err = os.ReadFile(filepath.Join(shared, "long", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "description: "+strings.Repeat("x", 1024)+"\n") {
		t.Fatalf("long description not cut at 1024: %q", got[:80])
	}
}

// Bad uploads are refused with the reason and nothing is written.
func TestImportSkill_Refusals(t *testing.T) {
	mux, shared := skillsMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/skills/import", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "Invalid multipart form: ") {
		t.Fatalf("not multipart = %d %q", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, field, filename, content, want string
	}{
		{"no file part", "other", "a.md", "x", "file is required\n"},
		{"too large", "file", "big.md", strings.Repeat("a", 1<<20+1), "file exceeds 1MB limit\n"},
		{"no usable name", "file", "___.md", "body", "skill name is required in frontmatter or filename\n"},
		{"name too long", "file", strings.Repeat("a", 65) + ".md", "body", "skill name exceeds 64 characters\n"},
	} {
		rec := importSkill(t, mux, tc.field, tc.filename, tc.content)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != tc.want {
			t.Errorf("%s = %d %q, want 400 %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if entries, err := os.ReadDir(shared); err == nil && len(entries) != 0 {
		t.Fatalf("refused imports wrote %d entries", len(entries))
	}
}

// When the skills folder cannot be created the import is a 500.
func TestImportSkill_FolderFailure(t *testing.T) {
	mux, shared := skillsMux(t)
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := importSkill(t, mux, "file", "plain.md", "body")
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Body.String(), "Failed to create skill directory: ") {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
}

// An unknown skill is a 404 for both reading and deleting.
func TestSkill_NotFound(t *testing.T) {
	mux, _ := skillsMux(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := serve(mux, method, "/api/skills/nope", "")
		if rec.Code != http.StatusNotFound || rec.Body.String() != "Skill not found\n" {
			t.Errorf("%s = %d %q", method, rec.Code, rec.Body.String())
		}
	}
}
