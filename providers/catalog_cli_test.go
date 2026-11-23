// ClawEh
// License: MIT

package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/config"
)

// cliScript writes a stand-in Claude CLI that records its arguments and
// environment in dir and answers with reply (empty: no text, as a CLI that
// refused a tool call does).
func cliScript(t *testing.T, dir, reply string) string {
	t.Helper()
	script := filepath.Join(dir, "claude")
	body := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > '%[1]s/args'
env > '%[1]s/env'
cat > /dev/null
printf '{"type":"result","result":"%[2]s","session_id":"t"}\n'
`, dir, reply)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// callCLI builds the catalogue of one claude-cli model on script and makes
// one chat call through its client.
func callCLI(t *testing.T, script string, bypass bool) error {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "Claude CLI", Protocol: "claude-cli", Command: script, BypassRestrictions: bypass}},
		Models: []config.ModelConfig{{
			ModelName: "c", Model: "claude-cli", Provider: "Claude CLI", Workspace: t.TempDir(), Enabled: true,
			Env: map[string]string{"ALICE_SETTING": "on"},
		}},
	}
	client, release, err := NewCatalogBuilder(nil, nil).Build(cfg).Client("c")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = client.Chat(context.Background(), testIdentity, []spawnllm.Message{{Role: "user", Content: "hi"}}, nil, "claude-cli", nil)
	return err
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A CLI model built by the catalogue runs with the permission-bypass flag
// only when its provider allows it, starts from the allowlisted environment
// (never the service's own variables) with the model's env on top, and
// without bypass turns a silent refusal into today's declined-tools error.
func TestCatalogue_CLIModelWiring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI")
	}
	t.Setenv("CLAW_TEST_SERVICE_SECRET", "do-not-pass")

	dir := t.TempDir()
	if err := callCLI(t, cliScript(t, dir, "hello"), true); err != nil {
		t.Fatalf("call with bypass: %v", err)
	}
	if args := readLines(t, filepath.Join(dir, "args")); !slices.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("bypass on: args %v lack the bypass flag", args)
	}
	env := strings.Join(readLines(t, filepath.Join(dir, "env")), "\n")
	if strings.Contains(env, "CLAW_TEST_SERVICE_SECRET") {
		t.Error("the CLI saw a variable outside the allowlist")
	}
	for _, want := range []string{"ALICE_SETTING=on", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "PATH="} {
		if !strings.Contains(env, want) {
			t.Errorf("the CLI's environment lacks %s", want)
		}
	}

	dir = t.TempDir()
	err := callCLI(t, cliScript(t, dir, ""), false)
	if args := readLines(t, filepath.Join(dir, "args")); slices.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("bypass off: args %v carry the bypass flag", args)
	}
	declined, ok := errors.AsType[*models.CLIDeclinedError](err)
	const want = "The Claude CLI declined to use tools. Tick *Allow CLI to bypass restrictions* for this CLI on the Providers page, or allow the tools in the CLI's own settings."
	if !ok || declined.Error() != want {
		t.Fatalf("empty answer without bypass: err = %v, want %q", err, want)
	}
}
