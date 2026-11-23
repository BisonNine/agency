// ClawEh
// License: MIT

package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
)

// postCardForm sends a multipart import with the given fields; a file part is
// added when file is non-nil.
func postCardForm(t *testing.T, mux *http.ServeMux, fields map[string]string, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if file != nil {
		part, err := w.CreateFormFile("file", "card.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/cards/import", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func cardErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Error
}

// An import without an id or a file, not multipart, too large, or not a card
// is refused with the reason and stores nothing.
func TestCards_ImportRefusals(t *testing.T) {
	mux, dir := cardsHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/cards/import", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(cardErr(t, rec), "Invalid multipart form: ") {
		t.Fatalf("not multipart = %d %q", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name   string
		fields map[string]string
		file   []byte
		want   string
	}{
		{"no id", map[string]string{"id": "  "}, []byte(watsonV3), "id is required"},
		{"no file", map[string]string{"id": "holmes"}, nil, "file is required"},
		{"too large", map[string]string{"id": "holmes"}, bytes.Repeat([]byte("a"), 8<<20+1), "The file is larger than 8 MiB."},
	} {
		got := postCardForm(t, mux, tc.fields, tc.file)
		if got.Code != http.StatusBadRequest || cardErr(t, got) != tc.want {
			t.Errorf("%s = %d %q, want 400 %q", tc.name, got.Code, got.Body.String(), tc.want)
		}
	}
	rec = postCardForm(t, mux, map[string]string{"id": "holmes"}, []byte(`{"not":"a card"}`))
	if rec.Code != http.StatusBadRequest || cardErr(t, rec) == "" {
		t.Fatalf("not a card = %d %q, want a 400 with the reason", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "holmes.json")); !os.IsNotExist(err) {
		t.Fatal("a refused import stored a card")
	}
}

// With the library folder gone, listing and importing fail with a 500 that
// names the failure.
func TestCards_LibraryGone(t *testing.T) {
	mux, dir := cardsHandler(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	rec := serve(mux, http.MethodGet, "/api/cards", "")
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(cardErr(t, rec), "list the card library: ") {
		t.Fatalf("list = %d %q", rec.Code, rec.Body.String())
	}
	rec = postCardForm(t, mux, map[string]string{"id": "holmes"}, []byte(watsonV3))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("import = %d %q, want 500", rec.Code, rec.Body.String())
	}
}

// A library that cannot be created fails every card endpoint with a 500.
func TestCards_LibraryCannotOpen(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	cfg, err := h.currentConfig()
	if err != nil {
		t.Fatal(err)
	}
	// A data folder the library cannot be created in.
	if err := os.MkdirAll(cfg.DataDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.DataDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(cfg.DataDir(), 0o700); err != nil {
			t.Error(err)
		}
	})
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions")
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/cards"},
		{http.MethodGet, "/api/cards/x"},
		{http.MethodDelete, "/api/cards/x"},
	} {
		rec := serve(mux, tc.method, tc.path, "")
		if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(cardErr(t, rec), "open the card library: ") {
			t.Errorf("%s %s = %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	rec := postCardForm(t, mux, map[string]string{"id": "x"}, []byte(watsonV3))
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(cardErr(t, rec), "open the card library: ") {
		t.Errorf("import = %d %q", rec.Code, rec.Body.String())
	}
}

// A card used by several agents names them all, by display name, when its
// deletion is refused; a card with no tags lists an empty tag list.
func TestCards_DeleteRefusedNamesEveryAgent(t *testing.T) {
	configPath := setupTestEnv(t)
	initTestAudit(t)
	cfg := loadSaved(t, configPath)
	cfg.Agents.List[0].CharacterCard = "holmes"
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob", CharacterCard: "Holmes"})
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	if rec := postCardForm(t, mux, map[string]string{"id": "holmes"}, []byte(watsonV3)); rec.Code != http.StatusOK {
		t.Fatalf("import = %d %q", rec.Code, rec.Body.String())
	}
	if err := h.updateConfig(nil, func(c *config.Config) error {
		c.Agents.List = cfg.Agents.List
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := serve(mux, http.MethodDelete, "/api/cards/holmes", "")
	if rec.Code != http.StatusConflict || cardErr(t, rec) != `Card "holmes" is attached to agents Main, Bob; detach it first.` {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodGet, "/api/cards", "")
	var resp cardsResponse
	decodeInto(t, rec, &resp)
	if len(resp.Cards) != 1 || resp.Cards[0].Tags == nil || len(resp.Cards[0].Tags) != 0 ||
		strings.Join(resp.Cards[0].AttachedTo, ",") != "main,bob" {
		t.Fatalf("cards = %+v", resp.Cards)
	}
}
