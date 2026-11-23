package common

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/tools/files"
	"go.bison9.com/agency/utils"
)

// scopeFixture is an agent whose file tools and common tools are registered
// from one config, as the gateway registers them, with a workspace holding a
// file in each of files/, skills/, sessions/, maestro/ and the root.
type scopeFixture struct {
	ws, common string
	fileDefs   []toolspec.ToolDefinition
	commonDefs []toolspec.ToolDefinition
}

func newScopeFixture(t *testing.T, readSubdirs []string, restrict bool, agentCfg *config.AgentConfig) *scopeFixture {
	t.Helper()
	f := &scopeFixture{ws: t.TempDir(), common: t.TempDir()}
	for _, p := range []string{"files/notes.md", "skills/s/SKILL.md", "sessions/alice.jsonl", "maestro/plan.md", "COGMEM.md"} {
		mustWriteFile(t, filepath.Join(f.ws, p), "body of "+p)
	}
	cfg := config.DefaultConfig()
	cfg.Agents.CommonDir = f.common
	cfg.Agents.Defaults.RestrictToWorkspace = restrict
	cfg.Agents.Defaults.WorkspaceReadSubdirs = readSubdirs
	deps := toolspec.Deps{Cfg: cfg, Host: tools.ToolDeps{Workspace: f.ws, AgentCfg: agentCfg}}
	t.Cleanup(func() {
		files.SetReadScopeSubdirs(nil)
		files.SetAllowedFoldersForWorkspace(f.ws, nil)
	})
	f.fileDefs = files.GlobalProvider.RegisterTools(deps)
	f.commonDefs = GlobalProvider.RegisterTools(deps)
	return f
}

// put runs common_put for path, stored in common as "out".
func (f *scopeFixture) put(t *testing.T, path string) *toolspec.Result {
	t.Helper()
	return call(t, findTool(t, f.commonDefs, "put"), map[string]any{"path": path, "as": "out"})
}

// readRefusal is the file tools' refusal for reading path.
func (f *scopeFixture) readRefusal(t *testing.T, path string) string {
	t.Helper()
	d := findTool(t, f.fileDefs, "read_lines")
	res, err := d.Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"path": path}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) {
		t.Fatalf("file_read_lines %s: want a refusal, got %v %q", path, res.IsError, res.ForLLM)
	}
	return res.Err.Error()
}

// wantCopied fails unless common_put copied path into common.
func (f *scopeFixture) wantCopied(t *testing.T, path string) {
	t.Helper()
	if res := f.put(t, path); res.IsError {
		t.Fatalf("put %s: %s", path, res.ForLLM)
	}
	got, err := os.ReadFile(filepath.Join(f.common, "out"))
	if err != nil || string(got) != "body of "+path {
		t.Errorf("put %s: common holds %q %v", path, got, err)
	}
	if err := os.Remove(filepath.Join(f.common, "out")); err != nil {
		t.Fatal(err)
	}
}

// wantRefused fails unless common_put refuses path with the file tools'
// words and nothing reaches common.
func (f *scopeFixture) wantRefused(t *testing.T, path string) {
	t.Helper()
	want := "common_put: " + f.readRefusal(t, path)
	res := f.put(t, path)
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || res.ForLLM != want {
		t.Errorf("put %s: want %q, got %v %q", path, want, res.IsError, res.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(f.common, "out")); !os.IsNotExist(err) {
		t.Errorf("put %s: a file reached common: %v", path, err)
	}
}

// common_put reads a source only if the agent's file tools could read it.
func TestPut_ReadScope(t *testing.T) {
	maestro := &config.AgentConfig{ID: "alice", Maestro: &config.MaestroConfig{Enabled: true}}
	tests := []struct {
		name     string
		subdirs  []string
		restrict bool
		agent    *config.AgentConfig
		copied   []string
		refused  []string
	}{
		{
			name: "default scope", subdirs: []string{"files", "skills"}, restrict: true,
			copied:  []string{"files/notes.md", "skills/s/SKILL.md"},
			refused: []string{"sessions/alice.jsonl", "COGMEM.md", "maestro/plan.md"},
		},
		{
			name: "wider scope", subdirs: []string{"files", "skills", "sessions"}, restrict: true,
			copied:  []string{"files/notes.md", "sessions/alice.jsonl"},
			refused: []string{"COGMEM.md"},
		},
		{
			name: "narrower scope", subdirs: []string{"files"}, restrict: true,
			copied:  []string{"files/notes.md"},
			refused: []string{"skills/s/SKILL.md", "sessions/alice.jsonl"},
		},
		{
			name: "agent with Maestro", subdirs: []string{"files", "skills"}, restrict: true, agent: maestro,
			copied:  []string{"files/notes.md", "maestro/plan.md"},
			refused: []string{"sessions/alice.jsonl"},
		},
		{
			name: "not confined to the workspace", subdirs: []string{"files"}, restrict: false,
			copied: []string{"files/notes.md", "sessions/alice.jsonl", "COGMEM.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newScopeFixture(t, tt.subdirs, tt.restrict, tt.agent)
			for _, p := range tt.copied {
				f.wantCopied(t, p)
			}
			for _, p := range tt.refused {
				f.wantRefused(t, p)
			}
		})
	}
}

// A link inside the read scope does not lead common_put outside it.
func TestPut_ReadScopeLinkRefused(t *testing.T) {
	f := newScopeFixture(t, []string{"files", "skills"}, true, nil)
	f.link(t, "../sessions/alice.jsonl", "files/session-link")
	f.link(t, "../sessions", "files/sessions")

	f.wantRefused(t, "files/session-link")
	f.wantRefused(t, "files/sessions/alice.jsonl")
}

func (f *scopeFixture) link(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(f.ws, at)); err != nil {
		t.Fatal(err)
	}
}

// sameFile tells the same file from another one.
func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *os.File {
		mustWriteFile(t, filepath.Join(dir, name), name)
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { utils.CloseQuietly(f) })
		return f
	}
	a, b := open("a"), open("b")
	if err := sameFile(a, a); err != nil {
		t.Errorf("same file: %v", err)
	}
	if err := sameFile(a, b); err == nil {
		t.Error("different files reported the same")
	}
	closed := open("c")
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sameFile(closed, a); err == nil {
		t.Error("closed first file")
	}
	if err := sameFile(a, closed); err == nil {
		t.Error("closed second file")
	}
}
