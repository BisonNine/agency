// Agency
// License: MIT

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
)

// The token prompt names the tools the way the turn's CLI shows them:
// OpenCode as claw_<tool>, every other protocol as mcp__claw__<tool>, in the
// text those have always had.
func TestSessionTokenLayer_PrefixPerProtocol(t *testing.T) {
	const claudeStyle = "# Session Token\n\nThe following token is confidential — never echo it to users or write it to files. " +
		"ALL `mcp__claw__*` tool calls MUST include the literal string below as the `session_token` parameter.\n\n" +
		"session_token: SST0123"
	for _, protocol := range []string{"", "claude-cli", "codex-cli", "antigravity-cli", "gemini-cli", "cursor-cli", "openai-chat"} {
		if got := sessionTokenLayer("SST0123", protocol).Text; got != claudeStyle {
			t.Errorf("%q: token layer = %q\nwant %q", protocol, got, claudeStyle)
		}
	}
	want := strings.Replace(claudeStyle, "`mcp__claw__*`", "`claw_*`", 1)
	if got := sessionTokenLayer("SST0123", "opencode-cli").Text; got != want {
		t.Errorf("opencode-cli: token layer = %q\nwant %q", got, want)
	}
}

// replyingCLI writes a stand-in CLI named name that records its standard
// input in logDir and prints answer.
func replyingCLI(t *testing.T, logDir, name, answer string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), name)
	body := "#!/bin/sh\ncat > '" + logDir + "/stdin'\ncat <<'EOF'\n" + answer + "\nEOF\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// A turn on a CLI model gives the CLI the token prompt with that CLI's tool
// prefix, chosen from the model the turn runs on.
func TestTurn_TokenPromptUsesTheCLIsToolPrefix(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	tests := []struct {
		protocol, binary, answer, want, notWant string
	}{
		{
			protocol: "opencode-cli", binary: "opencode",
			answer: `{"type":"text","sessionID":"s","part":{"id":"p","type":"text","text":"done"}}` + "\n" +
				`{"type":"step_finish","sessionID":"s","part":{"id":"f","type":"step-finish","reason":"stop","cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
			want: "ALL `claw_*` tool calls", notWant: "mcp__claw__",
		},
		{
			protocol: "claude-cli", binary: "claude",
			answer: `{"type":"result","result":"done","session_id":"s"}`,
			want:   "ALL `mcp__claw__*` tool calls", notWant: "`claw_*`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.protocol, func(t *testing.T) {
			logDir := t.TempDir()
			cfg := ownerTestConfig(t)
			cfg.Providers = []modelspec.Provider{{Name: "CLI", Protocol: tc.protocol, Command: replyingCLI(t, logDir, tc.binary, tc.answer)}}
			cfg.Models = []modelspec.Model{{ModelName: "m", Model: tc.protocol, Provider: "CLI", Enabled: true}}
			cfg.Agents.Defaults.Models = []string{"m"}
			if err := os.MkdirAll(cfg.CLIPath(), 0o700); err != nil {
				t.Fatal(err)
			}
			msgBus := bus.NewMessageBus()
			al := mustNewAgentLoop(t, cfg, msgBus, &recordingProvider{}, realClients{}, OwnsDataDir())
			al.SetSessionTokenIssuer(&countingSTI{})

			dispatch(al, inbound("c1", "id1", "hello"))
			nextOutbound(t, msgBus)
			al.activeRequests.Wait()

			stdin, err := os.ReadFile(filepath.Join(logDir, "stdin"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stdin), tc.want) || !strings.Contains(string(stdin), "session_token: SST-") {
				t.Errorf("the CLI's prompt lacks %q and its token:\n%s", tc.want, stdin)
			}
			if strings.Contains(string(stdin), tc.notWant) {
				t.Errorf("the CLI's prompt names %q", tc.notWant)
			}
		})
	}
}
