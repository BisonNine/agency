// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package mcpreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.bison9.com/agency/global"
)

// entry is what a CLI's configuration holds under Key.
type entry struct {
	found   bool
	address string // the URL, or the command of a non-HTTP entry
}

// cli is one row of the table: how to read the CLI's entry, how to register
// it, and the prefix its models see the host's tools under.
type cli struct {
	read func(ctx context.Context, r *Registrar, bin string) (entry, error)
	// register writes the entry; existing says the CLI has one now.
	register   func(ctx context.Context, r *Registrar, bin, url string, existing bool) error
	toolPrefix string
}

// clis is the table, keyed by modelspec protocol. set-mcp.sh and the manual
// (manual/providers/cli-providers.md, manual/mcp.md) give the same commands.
var clis = map[string]cli{
	"claude-cli": {
		read:       readClaude,
		register:   registerClaude,
		toolPrefix: global.HostMCPToolPrefix,
	},
	"codex-cli": {
		read:       readCodex,
		register:   registerCodex,
		toolPrefix: global.HostMCPToolPrefix,
	},
	"antigravity-cli": {
		read:       readAntigravity,
		register:   registerAntigravity,
		toolPrefix: global.HostMCPToolPrefix,
	},
	"cursor-cli": {
		read:       readCursor,
		register:   registerCursor,
		toolPrefix: global.HostMCPToolPrefix,
	},
	"opencode-cli": {
		read:       readOpenCode,
		register:   registerOpenCode,
		toolPrefix: global.HostMCPOpenCodeToolPrefix,
	},
}

// errUnreadable is a CLI answer this package cannot read with confidence; the
// state is then NotChecked rather than a guess.
var errUnreadable = errors.New("output not understood")

// commandError names the command that failed and the start of what it said.
func commandError(bin string, args []string, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return fmt.Errorf("%s %s: %w", filepath.Base(bin), strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), err, msg)
}

// runChecked runs bin and turns a failure into a commandError.
func runChecked(ctx context.Context, r *Registrar, bin string, args ...string) ([]byte, error) {
	out, stderr, err := r.exec(ctx, bin, args...)
	if err != nil {
		return out, commandError(bin, args, stderr, err)
	}
	return out, nil
}

// --- Claude Code --------------------------------------------------------

// readClaude runs `claude mcp get agency`, which prints the entry as
// "  URL: <url>" (HTTP) or "  Command: <cmd>" (stdio) lines, or fails with
// "No MCP server found with name: agency".
func readClaude(ctx context.Context, r *Registrar, bin string) (entry, error) {
	args := []string{"mcp", "get", Key}
	out, stderr, err := r.exec(ctx, bin, args...)
	if err != nil {
		if strings.Contains(string(out)+string(stderr), "No MCP server found") {
			return entry{}, nil
		}
		return entry{}, commandError(bin, args, stderr, err)
	}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		for _, field := range []string{"URL:", "Command:"} {
			if v, ok := strings.CutPrefix(line, field); ok && strings.TrimSpace(v) != "" {
				return entry{found: true, address: strings.TrimSpace(v)}, nil
			}
		}
	}
	return entry{}, errUnreadable
}

// registerClaude replaces the user-scope entry: `claude mcp add` refuses a
// name that exists, so an existing one is removed first.
func registerClaude(ctx context.Context, r *Registrar, bin, url string, existing bool) error {
	if existing {
		if _, err := runChecked(ctx, r, bin, "mcp", "remove", Key, "--scope", "user"); err != nil {
			return err
		}
	}
	_, err := runChecked(ctx, r, bin, "mcp", "add", "--transport", "http", Key, "--scope", "user", url)
	return err
}

// --- Codex CLI ----------------------------------------------------------

// readCodex reads `codex mcp list --json`: a list of servers, each with its
// name and transport (url for HTTP, command for stdio).
func readCodex(ctx context.Context, r *Registrar, bin string) (entry, error) {
	out, err := runChecked(ctx, r, bin, "mcp", "list", "--json")
	if err != nil {
		return entry{}, err
	}
	var servers []struct {
		Name      string `json:"name"`
		Transport struct {
			URL     string `json:"url"`
			Command string `json:"command"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(out, &servers); err != nil {
		return entry{}, fmt.Errorf("codex mcp list --json: %w", errUnreadable)
	}
	for _, s := range servers {
		if s.Name != Key {
			continue
		}
		if addr := firstNonEmpty(s.Transport.URL, s.Transport.Command); addr != "" {
			return entry{found: true, address: addr}, nil
		}
		return entry{}, errUnreadable
	}
	return entry{}, nil
}

// registerCodex removes an existing entry, then adds the HTTP one.
func registerCodex(ctx context.Context, r *Registrar, bin, url string, existing bool) error {
	if existing {
		if _, err := runChecked(ctx, r, bin, "mcp", "remove", Key); err != nil {
			return err
		}
	}
	_, err := runChecked(ctx, r, bin, "mcp", "add", Key, "--url", url)
	return err
}

// --- Antigravity --------------------------------------------------------

// urlPattern finds URLs in human output.
var urlPattern = regexp.MustCompile(`https?://[^\s"'<>(),\[\]]+`)

// readAntigravity reads `agy mcp list`, which has no machine-readable form.
// "No MCP servers configured." is none; a listing that never names Key does
// not have it; the entry's own line (Key as its first word) and the lines
// indented under it must hold exactly one URL. Anything else is unreadable.
func readAntigravity(ctx context.Context, r *Registrar, bin string) (entry, error) {
	out, err := runChecked(ctx, r, bin, "mcp", "list")
	if err != nil {
		return entry{}, err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return entry{}, errUnreadable
	}
	if strings.Contains(text, "No MCP servers configured") {
		return entry{}, nil
	}
	lines := strings.Split(text, "\n")
	start := -1
	mentions := 0
	for i, line := range lines {
		if keyWord.MatchString(line) {
			mentions++
		}
		if start < 0 && firstWord(line) == Key {
			start = i
		}
	}
	if mentions == 0 {
		return entry{}, nil
	}
	if start < 0 {
		return entry{}, errUnreadable
	}
	block := []string{lines[start]}
	indent := indentOf(lines[start])
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" || indentOf(line) <= indent {
			break
		}
		block = append(block, line)
	}
	urls := map[string]bool{}
	for _, line := range block {
		for _, u := range urlPattern.FindAllString(line, -1) {
			urls[u] = true
		}
	}
	if len(urls) != 1 {
		return entry{}, errUnreadable
	}
	for u := range urls {
		return entry{found: true, address: u}, nil
	}
	return entry{}, errUnreadable
}

// registerAntigravity adds or updates the entry; the type flag must come
// before the name.
func registerAntigravity(ctx context.Context, r *Registrar, bin, url string, _ bool) error {
	_, err := runChecked(ctx, r, bin, "mcp", "add", "--type", "http", Key, url)
	return err
}

// firstWord is a line's first word, without a list bullet or a trailing
// colon.
func firstWord(line string) string {
	line = strings.TrimLeft(line, " \t-*•")
	if i := strings.IndexAny(line, " \t:(,"); i >= 0 {
		line = line[:i]
	}
	return line
}

// keyWord matches Key as a whole word.
var keyWord = regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(Key) + `($|[^A-Za-z0-9_-])`)

// indentOf is the number of leading spaces and tabs.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// --- Cursor -------------------------------------------------------------

// cursorFile is Cursor's user-wide MCP configuration.
func (r *Registrar) cursorFile() string {
	return filepath.Join(r.home, ".cursor", "mcp.json")
}

// readCursor reads mcpServers.agency from ~/.cursor/mcp.json (Cursor has no
// mcp command). A missing file has no entry.
func readCursor(_ context.Context, r *Registrar, _ string) (entry, error) {
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	ok, err := readJSONFile(r.cursorFile(), &doc)
	if err != nil || !ok {
		return entry{}, err
	}
	return serverEntry(doc.Servers[Key])
}

// registerCursor sets mcpServers.agency to {"url": url} in ~/.cursor/mcp.json,
// keeping everything else in the file. A missing file (and folder) is created
// for the service user only; a file that does not parse is left alone.
func registerCursor(_ context.Context, r *Registrar, _, url string, _ bool) error {
	quoted, err := json.Marshal(url)
	if err != nil {
		return err
	}
	return editJSONFile(r.cursorFile(), []string{"mcpServers", Key}, json.RawMessage(`{"url":`+string(quoted)+`}`), true)
}

// --- OpenCode -----------------------------------------------------------

// openCodeFile is OpenCode's global configuration, the file
// `opencode mcp add --global` writes.
func (r *Registrar) openCodeFile() string {
	base := envValue(r.env, "XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(r.home, ".config")
	}
	return filepath.Join(base, "opencode", "opencode.json")
}

// readOpenCode reads mcp.servers.agency from OpenCode's global file.
// `opencode mcp list` is not used: it starts OpenCode's background service.
func readOpenCode(_ context.Context, r *Registrar, _ string) (entry, error) {
	var doc struct {
		MCP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"mcp"`
	}
	ok, err := readJSONFile(r.openCodeFile(), &doc)
	if err != nil || !ok {
		return entry{}, err
	}
	return serverEntry(doc.MCP.Servers[Key])
}

// openCodeAdded is the line `opencode mcp add` prints naming the file it wrote.
var openCodeAdded = regexp.MustCompile(`(?m)^MCP server "` + regexp.QuoteMeta(Key) + `" added to (.+)$`)

// registerOpenCode runs `opencode mcp add`, which replaces the whole entry
// and has no option for Code Mode, then sets "codemode": false on the entry
// in the file the command reports writing (its global file when it names
// none): with Code Mode on, the model must write a script to call a tool, and
// weaker models fail.
func registerOpenCode(ctx context.Context, r *Registrar, bin, url string, _ bool) error {
	out, err := runChecked(ctx, r, bin, "mcp", "add", Key, "--url", url, "--global")
	if err != nil {
		return err
	}
	file := r.openCodeFile()
	if m := openCodeAdded.FindStringSubmatch(string(out)); m != nil {
		if p := strings.TrimSpace(m[1]); filepath.IsAbs(p) {
			file = filepath.Clean(p)
		}
	}
	var doc struct {
		MCP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"mcp"`
	}
	if ok, err := readJSONFile(file, &doc); err != nil || !ok || len(doc.MCP.Servers[Key]) == 0 {
		return fmt.Errorf("turning Code Mode off: no %s entry in %s", Key, file)
	}
	if err := editJSONFile(file, []string{"mcp", "servers", Key, "codemode"}, json.RawMessage("false"), false); err != nil {
		return fmt.Errorf("turning Code Mode off: %w", err)
	}
	return nil
}

// --- shared -------------------------------------------------------------

// serverEntry reads one server object: its url (or Cursor's serverUrl), else
// its command (a string or a list).
func serverEntry(raw json.RawMessage) (entry, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return entry{}, nil
	}
	var s struct {
		URL       string          `json:"url"`
		ServerURL string          `json:"serverUrl"`
		Command   json.RawMessage `json:"command"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return entry{}, errUnreadable
	}
	if addr := firstNonEmpty(s.URL, s.ServerURL); addr != "" {
		return entry{found: true, address: addr}, nil
	}
	var cmd string
	if json.Unmarshal(s.Command, &cmd) == nil && cmd != "" {
		return entry{found: true, address: cmd}, nil
	}
	var argv []string
	if json.Unmarshal(s.Command, &argv) == nil && len(argv) > 0 {
		return entry{found: true, address: strings.Join(argv, " ")}, nil
	}
	return entry{}, errUnreadable
}

// readJSONFile decodes path into v. ok is false when the file does not
// exist; a file that cannot be read or parsed is an error.
func readJSONFile(path string, v any) (ok bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // a fixed CLI config path under the service user's home
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, errUnreadable)
	}
	return true, nil
}

func envValue(env []string, key string) string {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
