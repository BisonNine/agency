// ClawEh
// License: MIT

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/logger"
)

// reservedKeyMessage is the operator-facing wording of a reserved extra_body
// key: it names the product, as it always has.
func reservedKeyMessage(model, key string) string {
	return fmt.Sprintf("model %q: extra_body key %q collides with a request field %s manages",
		model, key, app.ShortName())
}

func TestValidateModel_ReservedKeyKeepsTheHostWording(t *testing.T) {
	m := modelspec.Model{
		ModelName: "alice-chat", Model: "gpt", Provider: "openai",
		ExtraBody: map[string]any{"temperature": 1},
	}
	err := ValidateModel(&m)
	if err == nil || err.Error() != reservedKeyMessage("alice-chat", "temperature") {
		t.Fatalf("ValidateModel = %v, want %q", err, reservedKeyMessage("alice-chat", "temperature"))
	}
}

func TestValidateModel_OtherErrorsPassThrough(t *testing.T) {
	ok := modelspec.Model{
		ModelName: "alice-chat", Model: "gpt", Provider: "openai",
		ExtraBody: map[string]any{"safety_settings": "strict"},
	}
	if err := ValidateModel(&ok); err != nil {
		t.Fatalf("valid model refused: %v", err)
	}
	bad := modelspec.Model{ModelName: "alice-chat", Model: "gpt"}
	if err := ValidateModel(&bad); err == nil || err.Error() != bad.Validate().Error() {
		t.Fatalf("ValidateModel = %v, want modelspec's %v", err, bad.Validate())
	}
}

// ValidateModels is what a config save (PUT /api/config) and a config reload
// run.
func TestValidateModels_ReservedKeyKeepsTheHostWording(t *testing.T) {
	cfg := &Config{
		Providers: []modelspec.Provider{{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.example.com/v1"}},
		Models: []modelspec.Model{{
			ModelName: "alice-chat", Model: "gpt", Provider: "openai",
			ExtraBody: map[string]any{"stream": true},
		}},
	}
	want := "models[0]: " + reservedKeyMessage("alice-chat", "stream")
	if err := cfg.ValidateModels(); err == nil || err.Error() != want {
		t.Fatalf("ValidateModels = %v, want %q", err, want)
	}
}

func TestPruneInvalid_ReservedKeyKeepsTheHostWording(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.RedirectForTest(&buf)
	defer restore()

	cfg := &Config{
		Providers: []modelspec.Provider{{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.example.com/v1"}},
		Models: []modelspec.Model{{
			ModelName: "bob-chat", Model: "gpt", Provider: "openai",
			ExtraBody: map[string]any{"tools": []any{}},
		}},
	}
	if _, dm := cfg.PruneInvalid(); dm != 1 {
		t.Fatalf("dropped %d models, want 1", dm)
	}
	var rec struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line: %v\n%s", err, buf.String())
	}
	if want := reservedKeyMessage("bob-chat", "tools"); rec.Reason != want {
		t.Fatalf("logged reason = %q, want %q", rec.Reason, want)
	}
}
