package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "common" || GlobalProvider.Description() != "Shared common-directory read/write operations" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}

func TestConfine(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		base, rel, want string
	}{
		{"", "a", "directory is not configured"},
		{base, "  ", "path is required"},
		{base, "/etc/passwd", `absolute paths are not allowed: "/etc/passwd"`},
		{base, "a/../../b", `path escapes the allowed directory: "a/../../b"`},
		{base, "..", `path escapes the allowed directory: ".."`},
	} {
		if _, err := confine(tc.base, tc.rel); err == nil || err.Error() != tc.want {
			t.Errorf("confine(%q, %q) = %v, want %q", tc.base, tc.rel, err, tc.want)
		}
	}
	if got, err := confine(base, " a/./b "); err != nil || got != filepath.Join(base, "a", "b") {
		t.Errorf("confine inside: %q %v", got, err)
	}
}

// Every tool refuses a missing argument, and one that is not text.
func TestMissingArguments(t *testing.T) {
	defs := GlobalProvider.RegisterTools(newDeps(t, t.TempDir(), t.TempDir(), nil))
	for tool, want := range map[string]string{
		"get":    "common_get: 'name' is required",
		"put":    "common_put: 'path' is required",
		"delete": "common_delete: 'name' is required",
	} {
		for _, args := range []map[string]any{{}, {"name": 7, "path": 7}} {
			if res := call(t, findTool(t, defs, tool), args); !res.IsError || res.ForLLM != want {
				t.Errorf("%s %v: %+v", tool, args, res)
			}
		}
	}
}

// Without a configuration there is no common directory and no workspace.
func TestNotConfigured(t *testing.T) {
	defs := GlobalProvider.RegisterTools(toolspec.Deps{})
	for tool, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"list": {map[string]any{}, "common_list: common directory is not configured"},
		"get":  {map[string]any{"name": "a"}, "common_get: workspace is not configured"},
		"put":  {map[string]any{"path": "a"}, "common_put: workspace is not configured"},
	} {
		if res := call(t, findTool(t, defs, tool), tc.args); !res.IsError || res.ForLLM != tc.want {
			t.Errorf("%s: %+v", tool, res)
		}
	}
}

func TestList_EdgeCases(t *testing.T) {
	commonDir := filepath.Join(t.TempDir(), "common")
	list := findTool(t, GlobalProvider.RegisterTools(newDeps(t, commonDir, t.TempDir(), nil)), "list")

	// Not created yet, then empty.
	if res := call(t, list, nil); res.IsError || res.ForLLM != "(empty)" {
		t.Errorf("missing: %+v", res)
	}
	if err := os.MkdirAll(filepath.Join(commonDir, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := call(t, list, map[string]any{"subdir": "sub/deeper"}); res.IsError || res.ForLLM != "(empty)" {
		t.Errorf("empty: %+v", res)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "sub", "f.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := call(t, list, map[string]any{"subdir": "sub"}); res.IsError || res.ForLLM != "deeper/\nf.txt\t3 bytes" {
		t.Errorf("listing: %+v", res)
	}
	// A file is not a folder to list.
	if res := call(t, list, map[string]any{"subdir": "sub/f.txt"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_list: ") {
		t.Errorf("file: %+v", res)
	}
}

func TestCopyFailures(t *testing.T) {
	commonDir := t.TempDir()
	workspace := t.TempDir()
	defs := GlobalProvider.RegisterTools(newDeps(t, commonDir, workspace, nil))

	if res := call(t, findTool(t, defs, "get"), map[string]any{"name": "missing.txt"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_get: copy failed: ") {
		t.Errorf("get missing: %+v", res)
	}
	if err := os.Mkdir(filepath.Join(commonDir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := call(t, findTool(t, defs, "get"), map[string]any{"name": "folder"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_get: copy failed: ") {
		t.Errorf("get a folder: %+v", res)
	}
	if res := call(t, findTool(t, defs, "put"), map[string]any{"path": "missing.txt"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_put: copy failed: ") {
		t.Errorf("put missing: %+v", res)
	}
	if res := call(t, findTool(t, defs, "delete"), map[string]any{"name": "missing.txt"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_delete: ") {
		t.Errorf("delete missing: %+v", res)
	}

	// The common directory cannot be created: a file is in its place.
	blocked := filepath.Join(t.TempDir(), "common")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	defs = GlobalProvider.RegisterTools(newDeps(t, blocked, workspace, nil))
	if res := call(t, findTool(t, defs, "put"), map[string]any{"path": "a.txt"}); !res.IsError || !strings.HasPrefix(res.ForLLM, "common_put: ") {
		t.Errorf("put into a blocked common dir: %+v", res)
	}
}

// copyFile's own failures: a destination folder that cannot be made, and a
// destination that cannot be opened.
func TestCopyFile_DestinationFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	modes := config.Modes{Dir: 0o755, File: 0o644}
	if err := copyFile(at(src), at(filepath.Join(src, "sub", "dst")), modes); err == nil {
		t.Error("destination under a file")
	}
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(at(src), at(filepath.Join(dir, "isdir")), modes); err == nil {
		t.Error("destination is a folder")
	}
	// A source that is a folder cannot be copied.
	if err := copyFile(at(filepath.Join(dir, "isdir")), at(filepath.Join(dir, "out")), modes); err == nil {
		t.Error("source is a folder")
	}
}
