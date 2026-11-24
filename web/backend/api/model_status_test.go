// Agency
// License: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.bison9.com/agent/modelspec"
)

func TestHasLocalAPIBase(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"http://localhost:11434/v1", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://0.0.0.0:8080", true},
		{"localhost:11434", true},
		{"127.0.0.1", true},
		{"https://api.openai.com/v1", false},
		{"api.example.com:443", false},
		{"http://%zz", false},
	} {
		if got := hasLocalAPIBase(tc.raw); got != tc.want {
			t.Errorf("hasLocalAPIBase(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestParseAPIBase(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		wantErr   bool
	}{
		{"", "", true},
		{"http://localhost:11434/v1", "http://localhost:11434/v1", false},
		{"localhost:11434", "http://localhost:11434", false},
		{"  example.com/v1 ", "http://example.com/v1", false},
		{"%zz", "", true},
	} {
		u, err := parseAPIBase(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAPIBase(%q) = %v, want an error", tc.raw, u)
			}
			continue
		}
		if err != nil || u.String() != tc.want {
			t.Errorf("parseAPIBase(%q) = %v, %v; want %q", tc.raw, u, err, tc.want)
		}
	}
}

// A wildcard bind address is probed on its loopback equivalent; anything else
// is left as it was.
func TestNormalizeModelProbeAPIBase(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://0.0.0.0:8080/v1", "http://127.0.0.1:8080/v1"},
		{"http://[::]:8080/v1", "http://[::1]:8080/v1"},
		{"http://0.0.0.0/v1", "http://127.0.0.1/v1"},
		{"0.0.0.0:11434", "http://127.0.0.1:11434"},
		{" http://localhost:8080/v1 ", "http://localhost:8080/v1"},
		{"", ""},
	} {
		if got := normalizeModelProbeAPIBase(tc.raw); got != tc.want {
			t.Errorf("normalizeModelProbeAPIBase(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A CLI provider is available without a probe; a remote provider is never
// probed as local.
func TestProbeLocalModelAvailability(t *testing.T) {
	resetModelProbeHooks(t)
	probes := 0
	probeOpenAICompatibleModelFunc = func(string, string) bool { probes++; return true }

	if !requiresRuntimeProbe(&modelspec.Provider{Protocol: "claude-cli"}) {
		t.Fatal("a CLI provider requires the runtime check")
	}
	if !probeLocalModelAvailability(&modelspec.Provider{Protocol: "claude-cli"}, modelspec.Model{Model: "sonnet"}) {
		t.Fatal("a CLI provider must count as available")
	}
	if probeLocalModelAvailability(&modelspec.Provider{Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1"}, modelspec.Model{Model: "gpt-4o"}) {
		t.Fatal("a remote provider must not count as a reachable local model")
	}
	if probes != 0 {
		t.Fatalf("probes = %d, want 0", probes)
	}
	if !probeLocalModelAvailability(&modelspec.Provider{Protocol: "openai-chat", BaseURL: "http://0.0.0.0:1/v1"}, modelspec.Model{Model: "m"}) || probes != 1 {
		t.Fatalf("local provider: probes = %d, want 1", probes)
	}
}

// The local probe GETs {base}/models and finds the model by id, ignoring case
// and surrounding space; a non-200, a body that is not JSON, an unreachable
// server and an empty base all count as unavailable.
func TestProbeOpenAICompatibleModel(t *testing.T) {
	var status int
	var body string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	status, body = http.StatusOK, `{"data":[{"id":"other"},{"id":" Llama3 "}]}`
	if !probeOpenAICompatibleModel(srv.URL+"/v1/", "llama3") {
		t.Fatal("listed model not found")
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if probeOpenAICompatibleModel(srv.URL+"/v1", "missing") {
		t.Fatal("unlisted model reported available")
	}
	status, body = http.StatusInternalServerError, `{}`
	if probeOpenAICompatibleModel(srv.URL+"/v1", "llama3") {
		t.Fatal("a 500 must count as unavailable")
	}
	status, body = http.StatusOK, `not json`
	if probeOpenAICompatibleModel(srv.URL+"/v1", "llama3") {
		t.Fatal("a body that is not JSON must count as unavailable")
	}
	if probeOpenAICompatibleModel("  ", "llama3") {
		t.Fatal("an empty base must count as unavailable")
	}
	if probeOpenAICompatibleModel("http://%zz", "llama3") {
		t.Fatal("an invalid base must count as unavailable")
	}
	srv.Close()
	if probeOpenAICompatibleModel(srv.URL+"/v1", "llama3") {
		t.Fatal("an unreachable server must count as unavailable")
	}
}
