// Agency
// License: MIT

package providers

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
)

// openCodeScript puts a stand-in `opencode` on PATH that records its
// arguments, environment and stdin in dir and answers with one model step
// of OpenCode's JSON event stream.
func openCodeScript(t *testing.T, dir string) {
	t.Helper()
	bin := t.TempDir()
	body := `#!/bin/sh
printf '%s\n' "$@" > '` + dir + `/args'
env > '` + dir + `/env'
cat > '` + dir + `/stdin'
echo '{"type":"step_start","sessionID":"ses_a","part":{"id":"prt_s","type":"step-start"}}'
echo '{"type":"text","sessionID":"ses_a","part":{"id":"prt_t","type":"text","text":"hello from opencode"}}'
echo '{"type":"step_finish","sessionID":"ses_a","part":{"id":"prt_f","type":"step-finish","reason":"stop","cost":0.25,"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":30,"write":0}}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// callOpenCode builds the catalogue of one opencode-cli model the way the
// host does (no command: the binary is found on PATH) and makes one call.
func callOpenCode(t *testing.T, m modelspec.Model, bypass bool) *spawnllm.LLMResponse {
	t.Helper()
	m.ModelName, m.Provider, m.Enabled, m.Workspace = "oc", "OpenCode CLI", true, t.TempDir()
	cfg := &config.Config{
		Providers: []modelspec.Provider{{Name: "OpenCode CLI", Protocol: "opencode-cli", BypassRestrictions: bypass}},
		Models:    []modelspec.Model{m},
	}
	client, release, err := NewCatalogBuilder(nil, nil).Build(cfg).Client("oc")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	resp, err := client.Chat(context.Background(), testIdentity, []spawnllm.Message{{Role: "user", Content: "hi"}}, nil, m.Model, nil)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return resp
}

// An OpenCode model runs like the other CLIs: the catalogue's arguments
// (--auto only with the bypass on), the model's extra_args, no model flag
// for the sentinel id, the allowlisted environment with OPENCODE_ variables,
// and the answer's text, tokens and cost read from the event stream.
func TestCatalogue_OpenCodeModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI")
	}
	t.Setenv("CLAW_TEST_SERVICE_SECRET", "do-not-pass")
	t.Setenv("OPENCODE_CONFIG", "/home/alice/.config/opencode/opencode.json")

	tests := []struct {
		name   string
		model  modelspec.Model
		bypass bool
		want   []string
	}{
		{
			name:   "sentinel, bypass on",
			model:  modelspec.Model{Model: "opencode-cli"},
			bypass: true,
			want:   []string{"run", "--format", "json", "--auto", "--standalone"},
		},
		{
			name:  "sentinel, bypass off",
			model: modelspec.Model{Model: "opencode-cli"},
			want:  []string{"run", "--format", "json", "--standalone"},
		},
		{
			name:  "named model, extra args, bypass flag stripped when off",
			model: modelspec.Model{Model: "provider-x/model-y", ExtraArgs: []string{"--auto", "--thinking"}},
			want:  []string{"run", "--format", "json", "--standalone", "--thinking", "--model", "provider-x/model-y"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			openCodeScript(t, dir)
			resp := callOpenCode(t, tc.model, tc.bypass)

			if args := readLines(t, filepath.Join(dir, "args")); !slices.Equal(args, tc.want) {
				t.Errorf("args = %v\nwant %v", args, tc.want)
			}
			stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stdin), "hi") {
				t.Errorf("the prompt was not piped on stdin: %q", stdin)
			}
			env := strings.Join(readLines(t, filepath.Join(dir, "env")), "\n")
			if strings.Contains(env, "CLAW_TEST_SERVICE_SECRET") {
				t.Error("the CLI saw a variable outside the allowlist")
			}
			if !strings.Contains(env, "OPENCODE_CONFIG=") {
				t.Error("the CLI's environment lacks OPENCODE_CONFIG")
			}

			if resp.Content != "hello from opencode" {
				t.Errorf("content = %q", resp.Content)
			}
			if resp.Usage == nil || resp.Usage.PromptTokens != 130 || resp.Usage.CompletionTokens != 25 {
				t.Errorf("usage = %+v, want prompt 130, completion 25", resp.Usage)
			}
			if resp.Status == nil || resp.Status.CostUSD == nil || *resp.Status.CostUSD != 0.25 {
				t.Errorf("status = %+v, want cost 0.25", resp.Status)
			}
		})
	}
}
