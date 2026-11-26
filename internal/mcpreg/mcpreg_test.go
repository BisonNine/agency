// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package mcpreg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

// Every test here runs fake CLIs from a private folder, with HOME and
// XDG_CONFIG_HOME in temporary folders and PATH limited to that folder and the
// system's: no real CLI and no real configuration is ever reached.

const (
	ours  = "http://127.0.0.1:5999/internal"
	other = "http://127.0.0.1:7000/internal"
)

// fakeCLI is one script for every fake CLI: it keeps its "configuration" in
// $HOME/.fake-<name> (the URL), logs its arguments to $CLI_LOG, and misbehaves
// as $FAKE_MODE says (fail, hang, garbage).
const fakeCLI = `#!/bin/sh
name=$(basename "$0")
state="$HOME/.fake-$name"
echo "$name $*" >> "$CLI_LOG"
case "$FAKE_MODE" in
fail) echo "boom from $name" >&2; exit 3 ;;
hang) exec sleep 30 ;;
garbage) echo "%%% not what you expected %%%"; exit 0 ;;
esac
case "$name $1 $2" in
"claude mcp get")
	if [ -f "$state" ]; then
		printf 'agency:\n  Scope: User config (available in all your projects)\n  Status: connected\n  Type: http\n  URL: %s\n' "$(cat "$state")"
	else
		echo "No MCP server found with name: agency" >&2; exit 1
	fi ;;
"claude mcp remove") [ -f "$state" ] || { echo "not found" >&2; exit 1; }; rm -f "$state" ;;
"claude mcp add")
	[ "$3 $4 $5 $6 $7" = "--transport http agency --scope user" ] || { echo "bad args" >&2; exit 2; }
	[ -f "$state" ] && { echo "MCP server agency already exists in user config" >&2; exit 1; }
	echo "$8" > "$state" ;;
"codex mcp list")
	[ "$3" = "--json" ] || exit 2
	if [ -f "$state" ]; then
		printf '[{"name":"files","transport":{"type":"stdio","command":"files-mcp"}},{"name":"agency","enabled":true,"transport":{"type":"streamable_http","url":"%s"}}]\n' "$(cat "$state")"
	else
		echo '[{"name":"files","transport":{"type":"stdio","command":"files-mcp"}}]'
	fi ;;
"codex mcp remove") rm -f "$state" ;;
"codex mcp add") [ "$3 $4" = "agency --url" ] || exit 2; echo "$5" > "$state" ;;
"agy mcp list")
	if [ -f "$state" ]; then
		printf 'NAME    TYPE   STATUS   COMMAND/URL\nagency  http   enabled  %s\nfiles   stdio  enabled  files-mcp --root /srv\n' "$(cat "$state")"
	else
		echo "No MCP servers configured."
	fi ;;
"agy mcp add")
	[ "$3 $4 $5" = "--type http agency" ] || { echo "flags must come before <name>" >&2; exit 2; }
	echo "$6" > "$state"; echo 'Added MCP server "agency" (http)' ;;
"opencode mcp add")
	[ "$3 $4" = "agency --url" ] && [ "$6" = "--global" ] || exit 2
	cfg="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/opencode.json"
	mkdir -p "$(dirname "$cfg")"
	if [ -f "$cfg" ] && [ -n "$OPENCODE_KEEP" ]; then :; else
		printf '{"$schema":"https://opencode.ai/config.json","model":"m","mcp":{"servers":{"agency":{"type":"remote","url":"%s","codemode":true}}}}\n' "$5" > "$cfg"
	fi
	[ -n "$OPENCODE_SILENT" ] || echo "MCP server \"agency\" added to $cfg" ;;
"cursor-agent mcp enable") [ "$3" = "agency" ] || exit 2; echo "Enabled and approved MCP server: agency" ;;
*) echo "unexpected: $name $*" >&2; exit 9 ;;
esac
`

type rig struct {
	t    *testing.T
	home string
	xdg  string
	bin  string
	log  string
	env  []string
}

func newRig(t *testing.T, mode string, extra ...string) *rig {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	g := &rig{t: t, home: t.TempDir(), xdg: t.TempDir(), bin: t.TempDir()}
	g.log = filepath.Join(g.home, "calls.log")
	for _, a := range modelspec.CLIAgents {
		if err := os.WriteFile(filepath.Join(g.bin, a.Binary), []byte(fakeCLI), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.env = append([]string{
		"PATH=" + g.bin + ":/usr/bin:/bin",
		"HOME=" + g.home,
		"XDG_CONFIG_HOME=" + g.xdg,
		"CLI_LOG=" + g.log,
		"FAKE_MODE=" + mode,
	}, extra...)
	return g
}

// lookIn finds a binary in dir only, never on the real PATH.
func lookIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
		return "", exec.ErrNotFound
	}
}

func (g *rig) registrar(opts ...Option) *Registrar {
	return New(append([]Option{WithHome(g.home), WithEnv(g.env), WithLookPath(lookIn(g.bin)), WithTimeout(10 * time.Second)}, opts...)...)
}

func (g *rig) setURL(binary, url string) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.home, ".fake-"+binary), []byte(url+"\n"), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) calls() string {
	b, err := os.ReadFile(g.log)
	if err != nil && !os.IsNotExist(err) {
		g.t.Fatal(err)
	}
	return string(b)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func (g *rig) openCodeFile() string { return filepath.Join(g.xdg, "opencode", "opencode.json") }
func (g *rig) cursorFile() string   { return filepath.Join(g.home, ".cursor", "mcp.json") }

func check(t *testing.T, r *Registrar, protocol, url string) Status {
	t.Helper()
	st, err := r.Check(t.Context(), protocol, "", url)
	if err != nil {
		t.Fatalf("Check(%s): %v", protocol, err)
	}
	return st
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTable_CoversTheCatalogue(t *testing.T) {
	for _, a := range modelspec.CLIAgents {
		if _, ok := clis[a.Protocol]; !ok {
			t.Errorf("no entry for %s", a.Protocol)
		}
	}
	if len(clis) != len(modelspec.CLIAgents) {
		t.Errorf("table has %d rows, catalogue %d CLIs", len(clis), len(modelspec.CLIAgents))
	}
}

func TestToolPrefix(t *testing.T) {
	if got := ToolPrefix("opencode-cli"); got != global.HostMCPOpenCodeToolPrefix {
		t.Errorf("opencode = %q", got)
	}
	for _, p := range []string{"claude-cli", "codex-cli", "antigravity-cli", "cursor-cli", "openai-chat"} {
		if got := ToolPrefix(p); got != global.HostMCPToolPrefix {
			t.Errorf("%s = %q", p, got)
		}
	}
}

func TestInternalURL(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCPHost.Enabled = true
	cfg.MCPHost.Listen = "127.0.0.1:6123"
	if got := InternalURL(cfg); got != "http://127.0.0.1:6123/internal" {
		t.Errorf("got %q", got)
	}
	cfg.MCPHost.Listen = " "
	if got := InternalURL(cfg); got != "http://127.0.0.1:5911/internal" {
		t.Errorf("default listen: got %q", got)
	}
	cfg.MCPHost.Listen = "[::1]:5912"
	if got := InternalURL(cfg); got != "http://[::1]:5912/internal" {
		t.Errorf("ipv6: got %q", got)
	}
	cfg.MCPHost.Enabled = false
	cfg.MCPHost.AutoEnable = false
	if got := InternalURL(cfg); got != "" {
		t.Errorf("host off: got %q", got)
	}
	if got := InternalURL(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
}

func TestNew_Defaults(t *testing.T) {
	r := New()
	if r.home == "" || r.env == nil || r.run == nil || r.lookPath == nil || r.timeout != DefaultTimeout {
		t.Errorf("defaults not set: %+v", r)
	}
}

// Every CLI, every state the fakes can be put in.
func TestCheckAndRegister_EveryCLI(t *testing.T) {
	for _, a := range modelspec.CLIAgents {
		t.Run(a.Protocol, func(t *testing.T) {
			g := newRig(t, "")
			r := g.registrar()
			ctx := t.Context()

			if st := check(t, r, a.Protocol, ours); st.State != NotRegistered || st.Label != a.Label || st.Protocol != a.Protocol {
				t.Fatalf("fresh: %+v", st)
			}
			st, err := r.Register(ctx, a.Protocol, "", ours, false)
			if err != nil || st.State != Registered {
				t.Fatalf("register: %+v %v\n%s", st, err, g.calls())
			}
			if got := check(t, r, a.Protocol, ours+"/"); got.State != Registered {
				t.Errorf("a trailing slash is the same address: %+v", got)
			}
			// The same CLI, registered elsewhere: named, and not replaced
			// without replace.
			st, err = r.Register(ctx, a.Protocol, "", other, false)
			if !errors.Is(err, ErrReplaceRequired) || st.State != OtherAddress || st.Address != ours {
				t.Fatalf("replace not required: %+v %v", st, err)
			}
			if got := check(t, r, a.Protocol, ours); got.State != Registered {
				t.Fatalf("refused replace changed the entry: %+v", got)
			}
			st, err = r.Register(ctx, a.Protocol, "", other, true)
			if err != nil || st.State != Registered {
				t.Fatalf("replace: %+v %v\n%s", st, err, g.calls())
			}
			if got := check(t, r, a.Protocol, ours); got.State != OtherAddress || got.Address != other {
				t.Errorf("after replace: %+v", got)
			}
			if got := check(t, r, a.Protocol, ""); got.State != HostOff {
				t.Errorf("host off: %+v", got)
			}
			if _, err := r.Register(ctx, a.Protocol, "", "", false); !errors.Is(err, ErrHostOff) {
				t.Errorf("register with host off: %v", err)
			}
		})
	}
}

func TestRegister_Commands(t *testing.T) {
	g := newRig(t, "")
	r := g.registrar()
	for _, a := range modelspec.CLIAgents {
		if _, err := r.Register(context.Background(), a.Protocol, "", ours, false); err != nil {
			t.Fatalf("%s: %v", a.Protocol, err)
		}
	}
	for _, want := range []string{
		"claude mcp add --transport http agency --scope user " + ours,
		"codex mcp add agency --url " + ours,
		"agy mcp add --type http agency " + ours,
		"opencode mcp add agency --url " + ours + " --global",
		"cursor-agent mcp enable agency",
	} {
		if !strings.Contains(g.calls(), want+"\n") {
			t.Errorf("no call %q in:\n%s", want, g.calls())
		}
	}
	if n := strings.Count(g.calls(), "cursor-agent "); n != 1 {
		t.Errorf("Cursor's binary was run %d times, want once (mcp enable):\n%s", n, g.calls())
	}
	if strings.Contains(g.calls(), "opencode mcp list") {
		t.Errorf("opencode mcp list was run:\n%s", g.calls())
	}
}

func TestCheck_NotInstalled(t *testing.T) {
	g := newRig(t, "")
	for _, a := range modelspec.CLIAgents {
		if err := os.Remove(filepath.Join(g.bin, a.Binary)); err != nil {
			t.Fatal(err)
		}
	}
	r := g.registrar()
	for _, st := range r.CheckAll(context.Background(), nil, ours) {
		if st.State != NotInstalled {
			t.Errorf("%+v", st)
		}
		if _, err := r.Register(context.Background(), st.Protocol, "", ours, true); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("%s register: %v", st.Protocol, err)
		}
	}
	if g.calls() != "" {
		t.Errorf("commands ran:\n%s", g.calls())
	}
}

func TestUnknownCLI(t *testing.T) {
	r := newRig(t, "").registrar()
	if _, err := r.Check(context.Background(), "openai-chat", "", ours); !errors.Is(err, ErrUnknownCLI) {
		t.Errorf("check: %v", err)
	}
	if _, err := r.Register(context.Background(), "nope; rm -rf /", "", ours, true); !errors.Is(err, ErrUnknownCLI) {
		t.Errorf("register: %v", err)
	}
}

func TestLookup_CatalogueEntryMissingFromTable(t *testing.T) {
	saved := clis["codex-cli"]
	delete(clis, "codex-cli")
	t.Cleanup(func() { clis["codex-cli"] = saved })
	if _, _, err := lookup("codex-cli"); !errors.Is(err, ErrUnknownCLI) {
		t.Errorf("got %v", err)
	}
}

// The gemini-cli alias is Antigravity.
func TestAlias(t *testing.T) {
	g := newRig(t, "")
	st := check(t, g.registrar(), "gemini-cli", ours)
	if st.Protocol != "antigravity-cli" || st.State != NotRegistered {
		t.Errorf("%+v", st)
	}
}

// A CLI whose command fails is Not checked, and registering it fails with the
// command named.
func TestCommandFailure(t *testing.T) {
	g := newRig(t, "fail")
	r := g.registrar()
	for _, p := range []string{"claude-cli", "codex-cli", "antigravity-cli"} {
		if st := check(t, r, p, ours); st.State != NotChecked {
			t.Errorf("%s: %+v", p, st)
		}
		_, err := r.Register(context.Background(), p, "", ours, true)
		if err == nil || !strings.Contains(err.Error(), "mcp add") || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s register: %v", p, err)
		}
	}
	_, err := r.Register(context.Background(), "opencode-cli", "", ours, false)
	if err == nil || !strings.Contains(err.Error(), "opencode mcp add") {
		t.Errorf("opencode register: %v", err)
	}
}

func TestCommandFailure_LongMessageCut(t *testing.T) {
	err := commandError("/x/claude", []string{"mcp"}, []byte(strings.Repeat("a", 500)), errors.New("exit 1"))
	if !strings.HasSuffix(err.Error(), "…") || len(err.Error()) > 260 {
		t.Errorf("%q", err)
	}
	if err := commandError("/x/claude", []string{"mcp"}, nil, errors.New("exit 1")); err.Error() != "claude mcp: exit 1" {
		t.Errorf("%q", err)
	}
}

func TestTimeout(t *testing.T) {
	g := newRig(t, "hang")
	r := g.registrar(WithTimeout(300 * time.Millisecond))
	start := time.Now()
	if st := check(t, r, "claude-cli", ours); st.State != NotChecked {
		t.Errorf("%+v", st)
	}
	_, err := r.Register(context.Background(), "codex-cli", "", ours, false)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("register: %v", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("took %v", d)
	}
}

// Output that cannot be read with confidence is Not checked, never a guess.
func TestUnreadableOutput(t *testing.T) {
	g := newRig(t, "garbage")
	r := g.registrar()
	for _, p := range []string{"claude-cli", "codex-cli"} {
		if st := check(t, r, p, ours); st.State != NotChecked {
			t.Errorf("%s: %+v", p, st)
		}
	}
	// agy: garbage that never names the server is a listing without it.
	if st := check(t, r, "antigravity-cli", ours); st.State != NotRegistered {
		t.Errorf("agy: %+v", st)
	}
}

func fixedOutput(out string) RunFunc {
	return func(context.Context, string, []string, string, ...string) ([]byte, []byte, error) {
		return []byte(out), nil, nil
	}
}

func readWith(t *testing.T, read func(context.Context, *Registrar, string) (entry, error), out string) (entry, error) {
	t.Helper()
	r := New(WithHome(t.TempDir()), WithEnv([]string{}), WithRunner(fixedOutput(out)))
	return read(context.Background(), r, "/fake/bin")
}

func TestReadClaude_Shapes(t *testing.T) {
	e, err := readWith(t, readClaude, "agency:\n  Type: stdio\n  Command: npx agency-mcp\n")
	if err != nil || !e.found || e.address != "npx agency-mcp" {
		t.Errorf("stdio: %+v %v", e, err)
	}
	if _, err := readWith(t, readClaude, "agency:\n  Type: http\n"); !errors.Is(err, errUnreadable) {
		t.Errorf("no url: %v", err)
	}
}

func TestReadCodex_Shapes(t *testing.T) {
	e, err := readWith(t, readCodex, `[{"name":"agency","transport":{"type":"stdio","command":"agency-mcp"}}]`)
	if err != nil || e.address != "agency-mcp" {
		t.Errorf("stdio: %+v %v", e, err)
	}
	if _, err := readWith(t, readCodex, `[{"name":"agency","transport":{}}]`); !errors.Is(err, errUnreadable) {
		t.Errorf("empty transport: %v", err)
	}
}

func TestReadAntigravity_Shapes(t *testing.T) {
	cases := []struct {
		name, out string
		want      entry
		err       bool
	}{
		{"one line", "- agency: " + ours + " (http)\n- files: npx x\n", entry{true, ours}, false},
		{"bullet", "• agency  " + ours, entry{true, ours}, false},
		{"block", "agency\n  type: http\n  url: " + ours + "\nfiles\n  url: http://x/y\n", entry{true, ours}, false},
		{"other servers only", "files (http): http://x/y\n", entry{}, false},
		{"none configured", "No MCP servers configured.\n", entry{}, false},
		{"empty", "  \n", entry{}, true},
		{"named but not first", "server for agency: " + ours, entry{}, true},
		{"no url", "agency (stdio)\n  command: agency-mcp\n", entry{}, true},
		{"two urls", "agency " + ours + " " + other, entry{}, true},
		{"word inside another", "my-agency-mcp: http://x/y", entry{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := readWith(t, readAntigravity, c.out)
			if (err != nil) != c.err || e != c.want {
				t.Errorf("got %+v %v, want %+v err=%v", e, err, c.want, c.err)
			}
		})
	}
}

func TestOpenCode_CodeModeOff(t *testing.T) {
	g := newRig(t, "")
	r := g.registrar()
	ctx := t.Context()
	if _, err := r.Register(ctx, "opencode-cli", "", ours, false); err != nil {
		t.Fatal(err)
	}
	assertCodeMode := func() {
		t.Helper()
		data, err := os.ReadFile(g.openCodeFile())
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			MCP struct {
				Servers map[string]map[string]any `json:"servers"`
			} `json:"mcp"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%v\n%s", err, data)
		}
		e := doc.MCP.Servers["agency"]
		if e["codemode"] != false || e["url"] == nil {
			t.Errorf("entry %v", e)
		}
		if !strings.HasPrefix(string(data), "{\n  \"$schema\"") || !strings.Contains(string(data), `"model": "m"`) {
			t.Errorf("rest of the file not kept in order:\n%s", data)
		}
	}
	assertCodeMode()
	// The add replaces the whole entry, Code Mode on again; registering again
	// turns it off again.
	st, err := r.Register(ctx, "opencode-cli", "", other, true)
	if err != nil || st.State != Registered {
		t.Fatalf("%+v %v", st, err)
	}
	assertCodeMode()
}

// Without the line naming the file, the global file is edited.
func TestOpenCode_NoFileLine(t *testing.T) {
	g := newRig(t, "", "OPENCODE_SILENT=1")
	if _, err := g.registrar().Register(context.Background(), "opencode-cli", "", ours, false); err != nil {
		t.Fatal(err)
	}
	if data := readFile(t, g.openCodeFile()); !strings.Contains(data, `"codemode": false`) {
		t.Errorf("%s", data)
	}
}

// Without XDG_CONFIG_HOME the global file is under ~/.config.
func TestOpenCode_HomeConfig(t *testing.T) {
	g := newRig(t, "")
	var env []string
	for _, kv := range g.env {
		if !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			env = append(env, kv)
		}
	}
	r := g.registrar(WithEnv(env))
	if _, err := r.Register(context.Background(), "opencode-cli", "", ours, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(g.home, ".config", "opencode", "opencode.json"))
	if err != nil || !strings.Contains(string(data), `"codemode": false`) {
		t.Errorf("%s %v", data, err)
	}
}

// An add that leaves no entry is an error, and nothing is invented.
func TestOpenCode_EntryMissingAfterAdd(t *testing.T) {
	g := newRig(t, "", "OPENCODE_KEEP=1")
	writeFile(t, g.openCodeFile(), `{"mcp":{"servers":{}}}`)
	_, err := g.registrar().Register(context.Background(), "opencode-cli", "", ours, false)
	if err == nil || !strings.Contains(err.Error(), "Code Mode") {
		t.Errorf("got %v", err)
	}
	if data := readFile(t, g.openCodeFile()); data != `{"mcp":{"servers":{}}}` {
		t.Errorf("file changed: %s", data)
	}
}

func TestOpenCode_Unreadable(t *testing.T) {
	g := newRig(t, "")
	r := g.registrar()
	writeFile(t, g.openCodeFile(), `{"mcp": nope`)
	if st := check(t, r, "opencode-cli", ours); st.State != NotChecked {
		t.Errorf("unparseable: %+v", st)
	}
	writeFile(t, g.openCodeFile(), `{"mcp":{"servers":{"agency":{"type":"local","command":["agency-mcp","--x"]}}}}`)
	if st := check(t, r, "opencode-cli", ours); st.State != OtherAddress || st.Address != "agency-mcp --x" {
		t.Errorf("local: %+v", st)
	}
	writeFile(t, g.openCodeFile(), `{"mcp":{"servers":{"agency":{"type":"remote"}}}}`)
	if st := check(t, r, "opencode-cli", ours); st.State != NotChecked {
		t.Errorf("no url: %+v", st)
	}
	writeFile(t, g.openCodeFile(), `{"mcp":{"servers":{"agency":"x"}}}`)
	if st := check(t, r, "opencode-cli", ours); st.State != NotChecked {
		t.Errorf("not an object: %+v", st)
	}
	if err := os.Remove(g.openCodeFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(g.openCodeFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if st := check(t, r, "opencode-cli", ours); st.State != NotChecked {
		t.Errorf("a folder: %+v", st)
	}
}

func TestCursor_File(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		g := newRig(t, "")
		if _, err := g.registrar().Register(t.Context(), "cursor-cli", "", ours, false); err != nil {
			t.Fatal(err)
		}
		if m := mode(t, g.cursorFile()); m != 0o600 {
			t.Errorf("file mode %v", m)
		}
		if m := mode(t, filepath.Dir(g.cursorFile())); m != 0o700 {
			t.Errorf("folder mode %v", m)
		}
		data := readFile(t, g.cursorFile())
		want := "{\n  \"mcpServers\": {\n    \"agency\": {\n      \"url\": \"" + ours + "\"\n    }\n  }\n}\n"
		if data != want {
			t.Errorf("got\n%s\nwant\n%s", data, want)
		}
	})

	t.Run("updated and preserved", func(t *testing.T) {
		g := newRig(t, "")
		writeFile(t, g.cursorFile(), `{"zeta":1,"mcpServers":{"files":{"command":"files-mcp","args":["-v"]},"agency":{"url":"`+other+`","headers":{"X":"y"}}},"alpha":[1,2.50,{"b":null}]}`)
		if err := os.Chmod(g.cursorFile(), 0o640); err != nil {
			t.Fatal(err)
		}
		r := g.registrar()
		if st := check(t, r, "cursor-cli", ours); st.State != OtherAddress || st.Address != other {
			t.Fatalf("%+v", st)
		}
		st, err := r.Register(t.Context(), "cursor-cli", "", ours, true)
		if err != nil || st.State != Registered {
			t.Fatalf("%+v %v", st, err)
		}
		s := readFile(t, g.cursorFile())
		if strings.Index(s, "zeta") > strings.Index(s, "mcpServers") || strings.Index(s, "mcpServers") > strings.Index(s, "alpha") {
			t.Errorf("order changed:\n%s", s)
		}
		for _, want := range []string{`"files-mcp"`, `2.50`, `"b": null`, `"url": "` + ours + `"`} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in\n%s", want, s)
			}
		}
		if strings.Contains(s, "headers") {
			t.Errorf("old entry kept:\n%s", s)
		}
		if m := mode(t, g.cursorFile()); m != 0o640 {
			t.Errorf("mode %v", m)
		}
	})

	t.Run("no mcpServers yet", func(t *testing.T) {
		g := newRig(t, "")
		writeFile(t, g.cursorFile(), `{"theme":"dark"}`)
		if _, err := g.registrar().Register(t.Context(), "cursor-cli", "", ours, false); err != nil {
			t.Fatal(err)
		}
		if data := readFile(t, g.cursorFile()); !strings.Contains(data, `"theme": "dark"`) || !strings.Contains(data, ours) {
			t.Errorf("%s", data)
		}
	})

	t.Run("serverUrl", func(t *testing.T) {
		g := newRig(t, "")
		writeFile(t, g.cursorFile(), `{"mcpServers":{"agency":{"serverUrl":"`+ours+`"}}}`)
		if st := check(t, g.registrar(), "cursor-cli", ours); st.State != Registered {
			t.Errorf("%+v", st)
		}
	})

	t.Run("link followed", func(t *testing.T) {
		g := newRig(t, "")
		linked := filepath.Join(g.home, "dotfiles", "mcp.json")
		writeFile(t, linked, `{"mcpServers":{}}`)
		if err := os.MkdirAll(filepath.Dir(g.cursorFile()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(linked, g.cursorFile()); err != nil {
			t.Fatal(err)
		}
		if _, err := g.registrar().Register(t.Context(), "cursor-cli", "", ours, false); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(g.cursorFile()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("link replaced by a file: %v", err)
		}
		if data := readFile(t, linked); !strings.Contains(data, ours) {
			t.Errorf("%s", data)
		}
	})

	for name, content := range map[string]string{
		"unparseable":           `{"mcpServers": {`,
		"not an object":         `[1,2]`,
		"servers not an object": `{"mcpServers": 3}`,
		"trailing data":         `{} {}`,
	} {
		t.Run("left alone: "+name, func(t *testing.T) {
			g := newRig(t, "")
			writeFile(t, g.cursorFile(), content)
			r := g.registrar()
			if st := check(t, r, "cursor-cli", ours); st.State != NotChecked {
				t.Errorf("state %+v", st)
			}
			if _, err := r.Register(t.Context(), "cursor-cli", "", ours, true); err == nil {
				t.Error("registered into a file that does not parse")
			}
			if data := readFile(t, g.cursorFile()); data != content {
				t.Errorf("file changed: %s", data)
			}
		})
	}

	t.Run("unreadable folder", func(t *testing.T) {
		g := newRig(t, "")
		if err := os.MkdirAll(g.cursorFile(), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := g.registrar().Register(t.Context(), "cursor-cli", "", ours, false); err == nil {
			t.Error("no error")
		}
	})
}

func TestEditJSONFile_NoCreate(t *testing.T) {
	err := editJSONFile(filepath.Join(t.TempDir(), "x.json"), []string{"a"}, json.RawMessage("1"), false)
	if err == nil {
		t.Error("created")
	}
}

func TestEditJSONFile_FolderCannotBeMade(t *testing.T) {
	dir := t.TempDir()
	block := filepath.Join(dir, "file")
	writeFile(t, block, "x")
	if err := editJSONFile(filepath.Join(block, "sub", "x.json"), []string{"a"}, json.RawMessage("1"), true); err == nil {
		t.Error("no error")
	}
}

func TestSetJSONMember_DuplicatesAndNesting(t *testing.T) {
	out, err := setJSONMember([]byte(`{"a":1,"a":2,"b":{"c":true}}`), []string{"a"}, json.RawMessage(`3`))
	if err != nil || string(out) != `{"a":3,"a":3,"b":{"c":true}}` {
		t.Errorf("%s %v", out, err)
	}
	if _, err := setJSONMember([]byte(`{"b":1}`), []string{"b", "c"}, json.RawMessage(`3`)); !errors.Is(err, errNotObject) {
		t.Errorf("nested scalar: %v", err)
	}
	if _, err := objectMembers([]byte(``)); err == nil {
		t.Error("empty")
	}
	if _, err := objectMembers([]byte(`{"a" 1}`)); err == nil {
		t.Error("no colon")
	}
	if _, err := objectMembers([]byte(`{"a":}`)); err == nil {
		t.Error("no value")
	}
	if _, err := objectMembers([]byte(`{"a":1`)); err == nil {
		t.Error("unterminated")
	}
}

func TestLimitedBuffer(t *testing.T) {
	var b limitedBuffer
	big := make([]byte, maxOutput+10)
	if n, err := b.Write(big); n != len(big) || err != nil {
		t.Fatal(n, err)
	}
	if n, err := b.Write([]byte("x")); n != 1 || err != nil || b.Len() != maxOutput {
		t.Errorf("len %d", b.Len())
	}
}

// Calls for one CLI are serialised; different CLIs run in parallel.
func TestRegister_Serialised(t *testing.T) {
	var running, peak atomic.Int32
	var mu sync.Mutex
	perCLI := map[string]int{}
	run := func(ctx context.Context, _ string, _ []string, path string, args ...string) ([]byte, []byte, error) {
		n := running.Add(1)
		defer running.Add(-1)
		mu.Lock()
		perCLI[filepath.Base(path)]++
		mu.Unlock()
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return []byte("No MCP servers configured."), nil, nil
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "agy"), "")
	r := New(WithHome(t.TempDir()), WithEnv([]string{}), WithRunner(run), WithLookPath(lookIn(bin)))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := r.Register(context.Background(), "antigravity-cli", "", ours, false); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Errorf("%d commands for one CLI ran at once", peak.Load())
	}
	if perCLI["agy"] != 12 { // read, add, read back, four times
		t.Errorf("calls %v", perCLI)
	}
}

func TestCheckAll_Order(t *testing.T) {
	g := newRig(t, "")
	g.setURL("claude", ours)
	g.setURL("codex", other)
	sts := g.registrar().CheckAll(context.Background(), nil, ours)
	if len(sts) != len(modelspec.CLIAgents) {
		t.Fatal(sts)
	}
	for i, a := range modelspec.CLIAgents {
		if sts[i].Protocol != a.Protocol {
			t.Errorf("%d: %s", i, sts[i].Protocol)
		}
	}
	if sts[0].State != Registered || sts[1].State != OtherAddress || sts[1].Address != other {
		t.Errorf("%+v", sts)
	}
}

// A failed approval is the registration's error: Cursor would not load the
// entry the file now holds.
func TestCursor_EnableFails(t *testing.T) {
	g := newRig(t, "fail")
	_, err := g.registrar().Register(t.Context(), "cursor-cli", "", ours, false)
	if err == nil || !strings.Contains(err.Error(), "cursor-agent mcp enable agency") {
		t.Fatalf("err = %v, want the failed enable command", err)
	}
}

// Without a home directory nothing is read or written.
func TestNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	g := newRig(t, "")
	r := New(WithEnv(g.env), WithLookPath(lookIn(g.bin)))
	if r.home != "" {
		t.Skip("the platform found a home directory anyway")
	}
	if st := check(t, r, "cursor-cli", ours); st.State != NotChecked {
		t.Errorf("%+v", st)
	}
	if _, err := r.Register(t.Context(), "cursor-cli", "", ours, false); err == nil {
		t.Error("registered with no home")
	}
	if g.calls() != "" {
		t.Errorf("commands ran:\n%s", g.calls())
	}
}

// A CLI in the catalogue but not in the table reads as not checked.
func TestCheckAll_EntryMissingFromTable(t *testing.T) {
	saved := clis["cursor-cli"]
	delete(clis, "cursor-cli")
	t.Cleanup(func() { clis["cursor-cli"] = saved })
	g := newRig(t, "")
	for _, st := range g.registrar().CheckAll(t.Context(), nil, ours) {
		if st.Protocol == "cursor-cli" && (st.State != NotChecked || st.Label != "Cursor CLI") {
			t.Errorf("%+v", st)
		}
	}
}

// A remove that fails stops the registration before the add.
func TestRegister_RemoveFails(t *testing.T) {
	for _, c := range []struct{ protocol, bin, list string }{
		{"claude-cli", "claude", "agency:\n  URL: " + other + "\n"},
		{"codex-cli", "codex", `[{"name":"agency","transport":{"url":"` + other + `"}}]`},
	} {
		t.Run(c.protocol, func(t *testing.T) {
			var calls []string
			run := func(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, []byte, error) {
				calls = append(calls, strings.Join(args, " "))
				if args[1] == "remove" {
					return nil, []byte("locked"), errors.New("exit status 1")
				}
				return []byte(c.list), nil, nil
			}
			bin := t.TempDir()
			writeFile(t, filepath.Join(bin, c.bin), "")
			r := New(WithHome(t.TempDir()), WithEnv([]string{}), WithRunner(run), WithLookPath(lookIn(bin)))
			_, err := r.Register(t.Context(), c.protocol, "", ours, true)
			if err == nil || !strings.Contains(err.Error(), "locked") {
				t.Errorf("got %v", err)
			}
			for _, call := range calls {
				if strings.Contains(call, " add ") {
					t.Errorf("added after a failed remove: %v", calls)
				}
			}
		})
	}
}

// fixture is a file captured from a real CLI (testdata/).
func fixture(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join("testdata", name))
}

// The output of a real `agy mcp list`, and tables in its shape.
func TestReadAntigravity_RealTable(t *testing.T) {
	const fixtureURL = "http://127.0.0.1:59999/internal"
	const header = "NAME    TYPE   STATUS    COMMAND/URL\n"
	cases := []struct {
		name, out string
		want      entry
		err       bool
	}{
		{"captured: one server", fixture(t, "agy-list-one.txt"), entry{true, fixtureURL}, false},
		{"captured: none", fixture(t, "agy-list-none.txt"), entry{}, false},
		{"other servers only", header + "files   stdio  enabled   files-mcp --root /srv\nweb     http   enabled   http://x/mcp\n", entry{}, false},
		{"name containing agency", header + "agency2 http   enabled   http://x/mcp\nmy-agency http enabled http://y/mcp\n", entry{}, false},
		{"among others", header + "agency2 http   enabled   http://x/mcp\nagency  http   enabled   " + fixtureURL + "\n", entry{true, fixtureURL}, false},
		{"disabled", header + "agency  http   disabled  " + fixtureURL + "\n", entry{true, fixtureURL}, false},
		{"stdio", header + "agency  stdio  enabled   agency-mcp --port 1\n", entry{true, "agency-mcp --port 1"}, false},
		{"row cut short", header + "agency  http\n", entry{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := readWith(t, readAntigravity, c.out)
			if (err != nil) != c.err || e != c.want {
				t.Errorf("got %+v %v, want %+v err=%v", e, err, c.want, c.err)
			}
		})
	}
	// A stdio entry is somebody else's server.
	g := newRig(t, "")
	writeFile(t, filepath.Join(g.bin, "agy"), "#!/bin/sh\nprintf '"+strings.ReplaceAll(header, "\n", "\\n")+"agency  stdio  enabled   agency-mcp\\n'\n")
	if err := os.Chmod(filepath.Join(g.bin, "agy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := check(t, g.registrar(), "antigravity-cli", ours); st.State != OtherAddress || st.Address != "agency-mcp" {
		t.Errorf("stdio: %+v", st)
	}
}

// The file a real `opencode mcp add --global` writes, and the older form.
func TestReadOpenCode_Forms(t *testing.T) {
	const fixtureURL = "http://127.0.0.1:59999/internal"
	g := newRig(t, "")
	r := g.registrar()
	cases := []struct {
		name, file string
		url        string
		want       State
		addr       string
	}{
		{"captured add", fixture(t, "opencode-added.json"), fixtureURL, Registered, ""},
		{"captured add, other instance", fixture(t, "opencode-added.json"), ours, OtherAddress, fixtureURL},
		{"older form", `{"mcp":{"agency":{"type":"remote","url":"` + fixtureURL + `","enabled":true}}}`, fixtureURL, Registered, ""},
		{"older form, other address", `{"mcp":{"agency":{"type":"remote","url":"` + other + `","enabled":true}}}`, ours, OtherAddress, other},
		{"servers without agency, older form with it", `{"mcp":{"servers":{"files":{"url":"http://x"}},"agency":{"url":"` + ours + `"}}}`, ours, Registered, ""},
		{"neither", `{"mcp":{"servers":{"files":{"url":"http://x"}},"files2":{"url":"http://y"}}}`, ours, NotRegistered, ""},
		{"servers not an object", `{"mcp":{"servers":[1]}}`, ours, NotChecked, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeFile(t, g.openCodeFile(), c.file)
			if st := check(t, r, "opencode-cli", c.url); st.State != c.want || st.Address != c.addr {
				t.Errorf("%+v", st)
			}
		})
	}
}

// A CLI is looked for by the command its provider names, and by the
// catalogue's binary when it names none.
func TestCheck_LooksForConfiguredCommand(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	r := New(WithHome(t.TempDir()), WithEnv([]string{}), WithLookPath(func(name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, name)
		return "", errors.New("not found")
	}))
	for _, command := range []string{"/srv/tools/opencode", ""} {
		st, err := r.Check(t.Context(), "opencode-cli", command, ours)
		if err != nil || st.State != NotInstalled {
			t.Fatalf("Check(%q) = %+v, %v, want not installed", command, st, err)
		}
		if _, err := r.Register(t.Context(), "opencode-cli", command, ours, false); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("Register(%q) error = %v, want ErrNotInstalled", command, err)
		}
	}
	r.CheckAll(t.Context(), map[string]string{"opencode-cli": "/srv/tools/opencode"}, "")
	want := []string{"/srv/tools/opencode", "/srv/tools/opencode", "opencode", "opencode"}
	if !slices.Equal(asked[:4], want) {
		t.Errorf("looked for %q, want %q", asked[:4], want)
	}
	if !slices.Contains(asked[4:], "/srv/tools/opencode") || slices.Contains(asked[4:], "opencode") {
		t.Errorf("CheckAll looked for %q, want the configured command for OpenCode", asked[4:])
	}
}
