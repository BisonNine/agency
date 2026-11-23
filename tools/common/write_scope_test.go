package common

import (
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

// get runs common_get for doc.md, written as as.
func (f *scopeFixture) get(t *testing.T, as string) *toolspec.Result {
	t.Helper()
	return call(t, findTool(t, f.commonDefs, "get"), map[string]any{"name": "doc.md", "as": as})
}

// wantGot fails unless common_get wrote as to the workspace path got,
// naming it in its answer.
func (f *scopeFixture) wantGot(t *testing.T, as, got string) {
	t.Helper()
	res := f.get(t, as)
	if want := `Copied "doc.md" to workspace ` + got; res.IsError || res.ForLLM != want {
		t.Fatalf("get as %s: want %q, got %q", as, want, res.ForLLM)
	}
	body, err := os.ReadFile(filepath.Join(f.ws, got))
	if err != nil || string(body) != "from common" {
		t.Errorf("get as %s: %s holds %q %v", as, got, body, err)
	}
}

// wantGetRefused fails unless common_get refuses to write as, which lands at
// the workspace path path, with file_write's refusal for path, and writes
// nothing there.
func (f *scopeFixture) wantGetRefused(t *testing.T, as, path string) {
	t.Helper()
	want := "common_get: " + writeRefusal(t, f.fileDefs, path)
	res := f.get(t, as)
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || res.ForLLM != want {
		t.Errorf("get as %s: want %q, got %v %q", as, want, res.IsError, res.ForLLM)
	}
	if body, err := os.ReadFile(filepath.Join(f.ws, path)); err == nil && string(body) == "from common" {
		t.Errorf("get as %s: %s written", as, path)
	}
}

// common_get writes only where the agent's file tools could write, in the
// configured write folder, which as is relative to.
func TestGet_WriteScope(t *testing.T) {
	tests := []struct {
		name     string
		subdir   string
		restrict bool
		links    map[string]string // link -> target, inside the workspace
		got      map[string]string // as -> where it lands
		refused  map[string]string // as -> the workspace path refused
		check    func(*testing.T, *scopeFixture)
	}{
		{
			name: "default", subdir: "files", restrict: true,
			links: map[string]string{"files/tasks": "../tasks", "files/alias": "sub"},
			got: map[string]string{
				"a.md":       "files/a.md",
				"sub/b.md":   "files/sub/b.md",
				"alias/c.md": "files/alias/c.md",
			},
			refused: map[string]string{"tasks/d.md": "files/tasks/d.md"},
		},
		{
			name: "another write folder", subdir: "out", restrict: true,
			links: map[string]string{"out/files": "../files", "out/up": ".."},
			got:   map[string]string{"a.md": "out/a.md", "sub/b.md": "out/sub/b.md"},
			refused: map[string]string{
				"files/c.md":    "out/files/c.md",
				"up/files/d.md": "out/up/files/d.md",
			},
			check: func(t *testing.T, f *scopeFixture) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(f.ws, "files", "a.md")); !os.IsNotExist(err) {
					t.Errorf("files/ written: %v", err)
				}
			},
		},
		{
			name: "whole workspace writable", subdir: "", restrict: true,
			links: map[string]string{"files/tasks": "../tasks"},
			got: map[string]string{
				"a.md":             "a.md",
				"files/b.md":       "files/b.md",
				"files/tasks/c.md": "files/tasks/c.md",
			},
			check: func(t *testing.T, f *scopeFixture) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(f.ws, "tasks", "c.md")); err != nil {
					t.Errorf("not written through the link: %v", err)
				}
			},
		},
		{
			name: "not confined to the workspace", subdir: "files", restrict: false,
			links: map[string]string{"files/tasks": "../tasks"},
			got:   map[string]string{"a.md": "files/a.md", "tasks/b.md": "files/tasks/b.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newScopeFixtureWith(t, nil, func(cfg *config.Config) {
				cfg.Agents.Defaults.RestrictToWorkspace = tt.restrict
				cfg.Agents.Defaults.WorkspaceWriteSubdir = tt.subdir
			})
			mustWriteFile(t, filepath.Join(f.common, "doc.md"), "from common")
			for _, dir := range []string{"tasks", "files/sub", "out"} {
				if err := os.MkdirAll(filepath.Join(f.ws, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for link, target := range tt.links {
				f.link(t, target, link)
			}
			for as, got := range tt.got {
				f.wantGot(t, as, got)
			}
			for as, path := range tt.refused {
				f.wantGetRefused(t, as, path)
			}
			if tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}
