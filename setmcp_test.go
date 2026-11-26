package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI is a stand-in for one AI CLI: it records its arguments, one call
// per line, in the log file. Every `mcp list` names the server, so the
// script's wait for OpenCode ends at once.
const fakeCLI = `#!/bin/sh
echo "$(basename "$0") $*" >> "$CLI_LOG"
if [ "$1 $2" = "mcp list" ]; then
	echo "agency"
fi
`

// set-mcp.sh, run against a temp HOME with fake CLIs on the PATH, registers
// the server under the key agency in every CLI, at the address given in
// AGENCY_MCP_URL, and writes and approves Cursor's entry.
func TestSetMCP_RegistersAgency(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	bin := t.TempDir()
	for _, name := range []string{"agy", "codex", "claude", "opencode", "cursor-agent"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeCLI), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logFile := filepath.Join(home, "calls.log")
	const url = "http://127.0.0.1:5999/internal"
	path := bin + ":/usr/bin:/bin"
	cmd := exec.Command("sh", "set-mcp.sh")
	cmd.Env = []string{"HOME=" + home, "PATH=" + path, "CLI_LOG=" + logFile, "AGENCY_MCP_URL=" + url}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("set-mcp.sh: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agy mcp add --type http agency " + url,
		"codex mcp add agency --url " + url,
		"claude mcp add --transport http agency --scope user " + url,
		"opencode mcp add --global --url " + url + " agency",
		"cursor-agent mcp enable agency",
	} {
		if !strings.Contains(string(calls), want+"\n") {
			t.Errorf("no call %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(strings.ReplaceAll(string(calls), "openclaw", ""), "claw") { // rename-guard:old-name
		t.Errorf("a CLI was given the old name:\n%s", calls)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Servers["agency"]["url"]; got != url {
		t.Errorf("Cursor url for agency = %v, want %s:\n%s\n%s", got, url, cfg, out)
	}
}
