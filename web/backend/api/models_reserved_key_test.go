package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/config"
)

// The operator sees the reserved extra_body key error with the product's name
// in it, whichever page refuses the model.
func reservedKeyMessage(model, key string) string {
	return fmt.Sprintf("model %q: extra_body key %q collides with a request field %s manages",
		model, key, app.ShortName())
}

func TestModelEndpoints_ReservedKeyKeepsTheHostWording(t *testing.T) {
	body := `{"model_name":"alice-chat","model":"gpt-4o","provider":"openai","enabled":true,"extra_body":{"temperature":1}}`
	want := "Validation error: " + reservedKeyMessage("alice-chat", "temperature")
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/models"},
		{http.MethodPut, "/api/models/0"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			h := NewHandler(setupTestEnv(t))
			mux := http.NewServeMux()
			h.RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			got := []string{strings.TrimSpace(rec.Body.String())}
			if tc.method == http.MethodPut {
				// The update answers with a JSON list of errors.
				var res struct {
					Errors []string `json:"errors"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
					t.Fatalf("body %q: %v", rec.Body.String(), err)
				}
				got = res.Errors
			}
			if !slices.Equal(got, []string{want}) {
				t.Fatalf("errors = %q, want %q", got, want)
			}
		})
	}
}

// validateConfig is what PUT /api/config runs before saving.
func TestValidateConfig_ReservedKeyKeepsTheHostWording(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Models = append(cfg.Models, modelspec.Model{
		ModelName: "bob-chat", Model: "gpt",
		Provider: cfg.Providers[0].Name, ExtraBody: map[string]any{"model": "x"},
	})
	want := fmt.Sprintf("models[%d]: %s", len(cfg.Models)-1, reservedKeyMessage("bob-chat", "model"))
	if errs := validateConfig(cfg); !slices.Contains(errs, want) {
		t.Fatalf("validateConfig = %q, want it to contain %q", errs, want)
	}
}
