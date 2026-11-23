// ClawEh
// License: MIT

package api

import (
	"net/http"
	"strings"
	"testing"
)

// Model writes naming a provider that does not exist, a body that is not
// JSON, a name already taken, or an over-long default name are refused and
// nothing is saved.
func TestModelEndpoints_Refusals(t *testing.T) {
	p, mux := providerMux(t)
	if rec := serve(mux, http.MethodPost, "/api/models", `{"model_name":"second","model":"gpt-4o-mini","provider":"openai","enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("seed second model: %d %q", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		contains                 string
	}{
		{"add unknown provider", http.MethodPost, "/api/models", `{"model_name":"x","model":"m","provider":"nope"}`, 400, `"status":"validation_error"`},
		{"update bad JSON", http.MethodPut, "/api/models/0", `{`, 400, "Invalid JSON: "},
		{"update invalid model", http.MethodPut, "/api/models/0", `{"model_name":""}`, 400, "Validation error: "},
		{"update unknown provider", http.MethodPut, "/api/models/0", `{"provider":"nope"}`, 400, "Validation error: "},
		{"update name taken", http.MethodPut, "/api/models/0", `{"model_name":"second"}`, 400, `Validation error: model name \"second\" already in use`},
		{"default bad JSON", http.MethodPost, "/api/models/default", `{`, 400, "Invalid JSON: "},
		{"default name too long", http.MethodPost, "/api/models/default", `{"model_name":"` + strings.Repeat("m", 201) + `"}`, 400, "model_name too long"},
	} {
		rec := serve(mux, tc.method, tc.path, tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("%s = %d %q, want %d containing %q", tc.name, rec.Code, rec.Body.String(), tc.status, tc.contains)
		}
	}

	cfg := loadSaved(t, p)
	if len(cfg.Models) != 2 || cfg.Models[0].ModelName != "custom-default" || cfg.Models[0].Provider != "openai" {
		t.Fatalf("refused writes changed the models: %+v", cfg.Models)
	}
}

// Renaming a model re-points the agent default chain that used it.
func TestUpdateModel_RenameRepointsReferences(t *testing.T) {
	p, mux := providerMux(t)
	rec := serve(mux, http.MethodPut, "/api/models/0", `{"model_name":"renamed"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}`+"\n" {
		t.Fatalf("rename = %d %q", rec.Code, rec.Body.String())
	}
	cfg := loadSaved(t, p)
	if cfg.Models[0].ModelName != "renamed" || cfg.Models[0].Model != "gpt-4o" {
		t.Fatalf("model = %+v", cfg.Models[0])
	}
	if got := cfg.Agents.Defaults.Models; len(got) != 1 || got[0] != "renamed" {
		t.Fatalf("default chain = %v, want [renamed]", got)
	}
}
