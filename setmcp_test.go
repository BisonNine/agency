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
// per line, in the log file. The opencode one also writes the config file and
// prints the line the real `opencode mcp add` prints.
const fakeCLI = `#!/bin/sh
echo "$(basename "$0") $*" >> "$CLI_LOG"
if [ "$(basename "$0")" = opencode ] && [ "$2" = add ]; then
	cfg="$HOME/.config/opencode/opencode.json"
	mkdir -p "$(dirname "$cfg")"
	printf '{"mcp":{"servers":{"%s":{"type":"remote","url":"%s","codemode":true}}}}\n' "$3" "$5" > "$cfg"
	echo "MCP server \"$3\" added to $cfg"
fi
`

// set-mcp.sh, run against a temp HOME with fake CLIs on the PATH, registers
// the server under the key agency in every CLI and turns OpenCode's Code
// Mode off for it.
func TestSetMCP_RegistersAgency(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	bin := t.TempDir()
	for _, name := range []string{"agy", "codex", "claude", "opencode"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeCLI), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logFile := filepath.Join(home, "calls.log")
	const url = "http://127.0.0.1:5999/internal"
	path := bin + ":/usr/bin:/bin"
	if py, err := exec.LookPath("python3"); err == nil {
		path += ":" + filepath.Dir(py)
	}
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
		"opencode mcp add agency --url " + url + " --global",
	} {
		if !strings.Contains(string(calls), want+"\n") {
			t.Errorf("no call %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(strings.ReplaceAll(string(calls), "openclaw", ""), "claw") { // rename-guard:old-name
		t.Errorf("a CLI was given the old name:\n%s", calls)
	}
	if _, lerr := exec.LookPath("python3"); lerr != nil {
		return
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.MCP.Servers["agency"]["codemode"]; got != false {
		t.Errorf("opencode codemode for agency = %v, want false:\n%s\n%s", got, cfg, out)
	}
}
