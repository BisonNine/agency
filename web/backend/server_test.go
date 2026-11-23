package webserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"go.bison9.com/agency/config"
)

// testConfigPath writes a default config into a temp CLAW_HOME.
func testConfigPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAW_HOME", filepath.Join(dir, ".claw"))
	p := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(p, config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	return p
}

// RegisterRoutes mounts the API, the manual and the SPA on one mux, on the
// store passed in or on one opened from the path.
func TestServer_RegisterRoutes(t *testing.T) {
	p := testConfigPath(t)
	store, err := config.NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"store": {Store: store, ListenPort: 18790},
		"path":  {ConfigPath: p, ListenPort: 18790, Public: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(opts)
			if s.APIHandler() == nil {
				t.Fatal("no API handler")
			}
			mux := http.NewServeMux()
			s.RegisterRoutes(mux)
			for _, tc := range []struct {
				path   string
				status int
			}{
				{"/api/system/version", http.StatusOK},
				{"/api/providers", http.StatusOK},
				{"/api/no-such-endpoint", http.StatusNotFound},
				{"/manual", http.StatusMovedPermanently},
				{"/agents", http.StatusOK},
			} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
				if rec.Code != tc.status {
					t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.status)
				}
			}
		})
	}
}

// The SPA serves nothing but GET and HEAD.
func TestSPA_OnlyGetAndHead(t *testing.T) {
	h := spaHandler(templatedFS())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s / = %d, want 404", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD / = %d, want 200", rec.Code)
	}
}

// A build whose index.html is not templated is served as a file for every
// client-side route.
func TestSPA_PlainIndex(t *testing.T) {
	fsys := fstest.MapFS{"index.html": {Data: []byte("<p>plain</p>")}}
	orig := templatedFiles
	templatedFiles = map[string]templatedFile{}
	t.Cleanup(func() { templatedFiles = orig })
	rr := get(t, spaHandler(fsys), "/agents")
	if rr.Code != http.StatusOK || rr.Body.String() != "<p>plain</p>" {
		t.Fatalf("GET /agents = %d %q", rr.Code, rr.Body.String())
	}
}

// A manual build without a not-found page answers a plain 404.
func TestManual_NotFoundWithoutPage(t *testing.T) {
	h := manualHandler(fstest.MapFS{"index.html": {Data: []byte("x")}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manual/missing.html", nil))
	if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
		t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
	}
}

// failingWriter records the status and fails every body write, as a client
// that has gone away does.
type failingWriter struct {
	header http.Header
	status int
}

func (f *failingWriter) Header() http.Header { return f.header }
func (f *failingWriter) WriteHeader(s int)   { f.status = s }
func (f *failingWriter) Write([]byte) (int, error) {
	return 0, os.ErrClosed
}

// The manual's not-found page still sends its 404 when the body cannot be
// written.
func TestManual_NotFoundPageWriteFails(t *testing.T) {
	w := &failingWriter{header: http.Header{}}
	serveManualNotFound(w, fstest.MapFS{"404.html": {Data: []byte("gone")}})
	if w.status != http.StatusNotFound || w.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("status %d content type %q", w.status, w.header.Get("Content-Type"))
	}
}
