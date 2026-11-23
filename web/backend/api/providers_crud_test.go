// ClawEh
// License: MIT

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agency/config"
)

// providerMux is a handler on the shared test config (provider "openai" used
// by model "custom-default") with every route registered.
func providerMux(t *testing.T) (string, *http.ServeMux) {
	t.Helper()
	p := setupTestEnv(t)
	h := NewHandler(p)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return p, mux
}

func loadSaved(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// DELETE /api/providers/{index}: a non-numeric index is a 400, one out of
// range a 404, a provider a model still uses a 409; otherwise it is removed.
func TestDeleteProvider(t *testing.T) {
	p, mux := providerMux(t)

	rec := serve(mux, http.MethodDelete, "/api/providers/x", "")
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "Invalid index\n" {
		t.Fatalf("bad index: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodDelete, "/api/providers/5", "")
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"errors":["Index 5 out of range"]}`+"\n" {
		t.Fatalf("out of range: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodDelete, "/api/providers/0", "")
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"errors":["provider \"openai\" is in use by model \"custom-default\""]}`+"\n" {
		t.Fatalf("in use: %d %q", rec.Code, rec.Body.String())
	}
	if n := len(loadSaved(t, p).Providers); n != 1 {
		t.Fatalf("refused delete changed the providers: %d", n)
	}

	rec = serve(mux, http.MethodPost, "/api/providers",
		`{"name":"spare","protocol":"openai-chat","base_url":"https://spare.example/v1","api_key":"sk-spare-0123456789"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"index":1,"status":"ok"}`+"\n" {
		t.Fatalf("add: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodDelete, "/api/providers/1", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}`+"\n" {
		t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
	}
	if got := loadSaved(t, p).Providers; len(got) != 1 || got[0].Name != "openai" {
		t.Fatalf("providers after delete = %+v, want only openai", got)
	}
}

// POST /api/providers refuses a body that is not JSON and a provider that
// fails validation, saving nothing.
func TestAddProvider_Refusals(t *testing.T) {
	p, mux := providerMux(t)
	rec := serve(mux, http.MethodPost, "/api/providers", "{")
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "Invalid JSON: ") {
		t.Fatalf("bad JSON: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodPost, "/api/providers", `{"name":"bad","protocol":"no-such-protocol"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"status":"validation_error"`) ||
		!strings.Contains(rec.Body.String(), "Validation error: ") {
		t.Fatalf("invalid provider: %d %q", rec.Code, rec.Body.String())
	}
	if n := len(loadSaved(t, p).Providers); n != 1 {
		t.Fatalf("refused add saved a provider: %d", n)
	}
}

// PUT /api/providers/{index}: a masked or empty key keeps the stored key, a
// rename re-points the models that used the old name, and bad input is
// refused without saving.
func TestUpdateProvider_KeepsKeyAndRenames(t *testing.T) {
	p, mux := providerMux(t)

	rec := putProvider(t, mux, "x", `{}`)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "Invalid index\n" {
		t.Fatalf("bad index: %d %q", rec.Code, rec.Body.String())
	}
	rec = putProvider(t, mux, "3", `{}`)
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"errors":["Index 3 out of range"]}`+"\n" {
		t.Fatalf("out of range: %d %q", rec.Code, rec.Body.String())
	}
	rec = putProvider(t, mux, "0", `{`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Invalid JSON: ") {
		t.Fatalf("bad JSON: %d %q", rec.Code, rec.Body.String())
	}
	rec = putProvider(t, mux, "0", `{"protocol":"no-such-protocol"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Validation error: ") {
		t.Fatalf("invalid: %d %q", rec.Code, rec.Body.String())
	}
	if got := loadSaved(t, p).Providers[0]; got.Protocol != "openai-chat" {
		t.Fatalf("refused update saved protocol %q", got.Protocol)
	}

	rec = putProvider(t, mux, "0", `{"name":"primary","api_key":"sk-d****ault"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}`+"\n" {
		t.Fatalf("rename: %d %q", rec.Code, rec.Body.String())
	}
	cfg := loadSaved(t, p)
	if cfg.Providers[0].Name != "primary" || cfg.Providers[0].APIKey != "sk-default" {
		t.Fatalf("saved provider = %q key %q, want primary with the stored key", cfg.Providers[0].Name, cfg.Providers[0].APIKey)
	}
	if cfg.Models[0].Provider != "primary" {
		t.Fatalf("model provider = %q, want it re-pointed to primary", cfg.Models[0].Provider)
	}

	rec = putProvider(t, mux, "0", `{"api_key":"sk-new-0123456789"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("new key: %d %q", rec.Code, rec.Body.String())
	}
	if got := loadSaved(t, p).Providers[0].APIKey; got != "sk-new-0123456789" {
		t.Fatalf("saved key = %q, want the new one", got)
	}
}

// POST /api/providers/test runs the live probe on what was sent and always
// answers 200 with the verdict.
func TestTestProvider_Endpoint(t *testing.T) {
	_, mux := providerMux(t)
	var gotHeaders http.Header
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(status)
	}))
	defer srv.Close()

	post := func(body string) providerTestResponse {
		t.Helper()
		rec := serve(mux, http.MethodPost, "/api/providers/test", body)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status = %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		var out providerTestResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := post("{"); got.OK || got.Message != "Invalid request body." {
		t.Fatalf("bad body = %+v", got)
	}
	got := post(`{"protocol":" OpenAI-Chat ","base_url":"` + srv.URL + `/","api_key":" sk-x "}`)
	if !got.OK || got.Message != "Key accepted (the endpoint is currently rate-limiting requests)." {
		t.Fatalf("429 = %+v", got)
	}
	if gotHeaders.Get("Authorization") != "Bearer sk-x" {
		t.Fatalf("Authorization = %q, want the trimmed key", gotHeaders.Get("Authorization"))
	}

	status = http.StatusInternalServerError
	got = post(`{"protocol":"anthropic","base_url":"` + srv.URL + `","api_key":"ak"}`)
	if got.OK || got.Message != "Unexpected response from the endpoint (HTTP 500)." {
		t.Fatalf("500 = %+v", got)
	}
	if gotHeaders.Get("X-Api-Key") != "ak" || gotHeaders.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("anthropic headers = %v", gotHeaders)
	}

	if got = post(`{"protocol":"anthropic"}`); got.OK || got.Message != "An API key is required to test this provider." {
		t.Fatalf("anthropic without key = %+v", got)
	}
	for _, proto := range []string{"anthropic", "openai-chat"} {
		got = post(`{"protocol":"` + proto + `","base_url":"http://%zz","api_key":"k"}`)
		if got.OK || got.Message != "The base URL is not a valid URL." {
			t.Fatalf("%s invalid URL = %+v", proto, got)
		}
	}

	srv.Close()
	got = post(`{"protocol":"openai-chat","base_url":"` + srv.URL + `"}`)
	if got.OK || !strings.HasPrefix(got.Message, "Could not reach "+srv.URL+" — check the base URL and your network.") {
		t.Fatalf("unreachable = %+v", got)
	}
}

// Every status class maps to its verdict.
func TestClassifyProbeStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		ok   bool
		msg  string
	}{
		{204, true, "Success — the endpoint accepted the key."},
		{429, true, "Key accepted (the endpoint is currently rate-limiting requests)."},
		{401, false, "Authentication failed — the API key was rejected."},
		{403, false, "Authentication failed — the API key was rejected."},
		{404, false, "Reached the server, but https://x/models was not found — check the base URL."},
		{302, false, "Unexpected response from the endpoint (HTTP 302)."},
	} {
		got := classifyProbeStatus(tc.code, "https://x")
		if got.OK != tc.ok || got.Message != tc.msg {
			t.Errorf("classifyProbeStatus(%d) = %+v, want %v %q", tc.code, got, tc.ok, tc.msg)
		}
	}
}
