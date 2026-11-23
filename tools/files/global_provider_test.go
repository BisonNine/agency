package files

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// The provider's metadata and the published tool set: names and which are in
// the default set. The names are released API.
func TestGlobalProvider_Definitions(t *testing.T) {
	if got := GlobalProvider.Namespace(); got != "file" {
		t.Errorf("Namespace = %q", got)
	}
	if got := GlobalProvider.Description(); got != "File read/write/list/edit operations" {
		t.Errorf("Description = %q", got)
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}

	wantDefault := map[string]bool{
		"read_bytes": false, "read_lines": true, "count": true, "view_image": true,
		"write": true, "list": true, "search_lines": true, "glob": false,
		"search_bytes": false, "edit": true, "append": true, "copy": true,
		"edit_lines": true, "edit_bytes": false, "insert_lines": true,
		"insert_bytes": false, "delete_lines": true, "delete_bytes": false,
		"delete": true, "move": true, "mkdir": true, "rmdir": true,
	}
	defs := GlobalProvider.RegisterTools(toolspec.Deps{})
	if len(defs) != len(wantDefault) {
		t.Errorf("got %d tools, want %d", len(defs), len(wantDefault))
	}
	for _, d := range defs {
		want, ok := wantDefault[d.Name]
		if !ok {
			t.Errorf("unexpected tool %q", d.Name)
			continue
		}
		if d.DefaultAllow == nil || *d.DefaultAllow != want {
			t.Errorf("%s: DefaultAllow = %v, want %v", d.Name, d.DefaultAllow, want)
		}
		if d.Category != "filesystem" || d.Description == "" || d.RawSchema == nil {
			t.Errorf("%s: category %q, description %q, schema %v", d.Name, d.Category, d.Description, d.RawSchema)
		}
	}
}

// Enumerated without a config (zero Deps), the tools that guard against a
// missing instance say they are not available instead of panicking.
func TestGlobalProvider_HandlersWithoutConfig(t *testing.T) {
	want := map[string]string{
		"search_lines": "file search is not available",
		"search_bytes": "file search is not available",
		"glob":         "file glob is not available",
		"edit_lines":   "file edit is not available",
		"delete_bytes": "file edit is not available",
	}
	for _, d := range GlobalProvider.RegisterTools(toolspec.Deps{}) {
		msg, ok := want[d.Name]
		if !ok {
			continue
		}
		res, err := d.Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{}})
		if err != nil || !res.IsError || res.ForLLM != msg {
			t.Errorf("%s: err %v, result %+v", d.Name, err, res)
		}
	}
}

// globalFixture registers the tools for a confined agent with the default
// scopes (writes in files/, reads in files/ and skills/ plus the always
// readable folders), one allowed folder and Maestro on.
func globalFixture(t *testing.T) (ws, allowed string, call func(name string, args map[string]any) *toolspec.Result) {
	t.Helper()
	t.Setenv(global.EnvVarHome, t.TempDir())
	ws = t.TempDir()
	allowed = t.TempDir()
	t.Cleanup(func() {
		SetReadScopeSubdirs(nil)
		SetAllowedFoldersForWorkspace(ws, nil)
	})

	cfg := config.DefaultConfig()
	agentCfg := &config.AgentConfig{
		ID: "alice",
		AllowedFolders: []config.AllowedFolderConfig{
			{Path: allowed},
			{Path: filepath.Join(allowed, "missing")}, // skipped: not a folder
			{Path: "relative/folder"},                 // skipped: not absolute
		},
		Maestro: &config.MaestroConfig{Enabled: true},
	}
	defs := GlobalProvider.RegisterTools(toolspec.Deps{
		Cfg:  cfg,
		Host: tools.ToolDeps{Cfg: cfg, AgentCfg: agentCfg, AgentID: "alice", Workspace: ws},
	})
	byName := map[string]toolspec.ToolDefinition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	call = func(name string, args map[string]any) *toolspec.Result {
		t.Helper()
		d, ok := byName[name]
		if !ok {
			t.Fatalf("tool %s not registered", name)
		}
		res, err := d.Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: args})
		if err != nil {
			t.Fatalf("%s: handler error %v", name, err)
		}
		return res
	}
	return ws, allowed, call
}

// Built from a real config, the tools are confined: the write folder and
// maestro/ are created, writes land only in files/ (or an allowed folder),
// reads only in the read scope, and every tool works on its own folder.
func TestGlobalProvider_ConfinedTools(t *testing.T) {
	ws, allowed, call := globalFixture(t)

	for _, dir := range []string{"files", "maestro"} {
		if info, err := os.Stat(filepath.Join(ws, dir)); err != nil || !info.IsDir() {
			t.Fatalf("%s/ not created: %v", dir, err)
		}
	}

	ok := func(name string, args map[string]any) string {
		t.Helper()
		res := call(name, args)
		if res.IsError {
			t.Fatalf("%s %v: %s", name, args, res.ForLLM)
		}
		return res.ForLLM
	}
	refused := func(name string, args map[string]any) {
		t.Helper()
		if res := call(name, args); !res.IsError {
			t.Errorf("%s %v: want a refusal, got %s", name, args, res.ForLLM)
		}
	}

	// Writes: files/ and the allowed folder, nothing else.
	ok("write", map[string]any{"path": "files/a.txt", "content": "one\ntwo\n"})
	ok("write", map[string]any{"path": filepath.Join(allowed, "b.txt"), "content": "bee"})
	refused("write", map[string]any{"path": "notes.txt", "content": "x"})
	refused("write", map[string]any{"path": "../escape.txt", "content": "x"})
	refused("write", map[string]any{"path": filepath.Join(t.TempDir(), "x.txt"), "content": "x"})
	if _, err := os.Stat(filepath.Join(ws, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("refused write left a file: %v", err)
	}

	// Reads: the read scope, the allowed folder; not sessions/.
	mustWrite(t, filepath.Join(ws, "sessions", "s.jsonl"), "secret")
	if out := ok("read_lines", map[string]any{"path": "files/a.txt"}); !strings.Contains(out, "two") {
		t.Errorf("read_lines: %s", out)
	}
	if out := ok("read_bytes", map[string]any{"path": filepath.Join(allowed, "b.txt")}); !strings.Contains(out, "bee") {
		t.Errorf("read_bytes: %s", out)
	}
	refused("read_lines", map[string]any{"path": "sessions/s.jsonl"})
	refused("list", map[string]any{"path": "sessions"})

	// The other tools, each on files/.
	ok("edit", map[string]any{"path": "files/a.txt", "old_text": "one", "new_text": "uno"})
	ok("append", map[string]any{"path": "files/a.txt", "content": "three\n"})
	ok("edit_lines", map[string]any{"path": "files/a.txt", "start": 1, "replace": "UNO"})
	ok("insert_lines", map[string]any{"path": "files/a.txt", "after_line": 0, "text": "zero"})
	ok("delete_lines", map[string]any{"path": "files/a.txt", "start": 1})
	ok("edit_bytes", map[string]any{"path": "files/a.txt", "start": 0, "replace": "u"})
	ok("insert_bytes", map[string]any{"path": "files/a.txt", "at_offset": 0, "text": "#"})
	ok("delete_bytes", map[string]any{"path": "files/a.txt", "start": 0})
	if got := mustReadString(t, filepath.Join(ws, "files", "a.txt")); got != "uNO\ntwo\nthree\n" {
		t.Errorf("after edits: %q", got)
	}
	if out := ok("count", map[string]any{"path": "files/a.txt"}); !strings.Contains(out, "3") {
		t.Errorf("count: %s", out)
	}
	ok("copy", map[string]any{"source_path": "files/a.txt", "destination_path": "files/c.txt"})
	ok("move", map[string]any{"source_path": "files/c.txt", "destination_path": "files/d.txt"})
	ok("delete", map[string]any{"path": "files/d.txt", "sure": true, "backup": false})
	ok("mkdir", map[string]any{"path": "files/sub"})
	ok("rmdir", map[string]any{"path": "files/sub", "sure": true})
	refused("mkdir", map[string]any{"path": "elsewhere"})
	if out := ok("list", map[string]any{"path": "files"}); !strings.Contains(out, "a.txt") {
		t.Errorf("list: %s", out)
	}
	if out := ok("search_lines", map[string]any{"query": "three", "path": "files"}); !strings.Contains(out, "a.txt") {
		t.Errorf("search_lines: %s", out)
	}
	if out := ok("search_bytes", map[string]any{"query": "three", "path": "files"}); !strings.Contains(out, "a.txt") {
		t.Errorf("search_bytes: %s", out)
	}
	if out := ok("glob", map[string]any{"pattern": "*.txt", "path": "files"}); !strings.Contains(out, "a.txt") {
		t.Errorf("glob: %s", out)
	}
	mustWriteBytes(t, filepath.Join(ws, "files", "p.png"), makePNG(t, 2, 2))
	if res := call("view_image", map[string]any{"path": "files/p.png"}); res.IsError || len(res.Images) != 1 {
		t.Errorf("view_image: %s", res.ForLLM)
	}

	if !slices.Contains(allowedFoldersForWorkspace(ws), allowed) ||
		!slices.Contains(allowedFoldersForWorkspace(ws), filepath.Join(ws, "maestro")) ||
		len(allowedFoldersForWorkspace(ws)) != 2 {
		t.Errorf("allowed folders = %v", allowedFoldersForWorkspace(ws))
	}
}

// A nil agent config has no extra folders.
func TestAgentFolders_NilAgent(t *testing.T) {
	if got := agentFolders(nil, t.TempDir()); got != nil {
		t.Errorf("agentFolders(nil) = %v", got)
	}
}

// When maestro/ cannot be created (a file is in the way), it is left out.
func TestAgentFolders_MaestroNotCreatable(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "maestro"), "not a folder")
	got := agentFolders(&config.AgentConfig{ID: "bob", Maestro: &config.MaestroConfig{Enabled: true}}, ws)
	if len(got) != 0 {
		t.Errorf("agentFolders = %v, want none", got)
	}
}

func mustReadString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustWriteBytes(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
