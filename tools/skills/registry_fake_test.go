package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	skillspkg "go.bison9.com/agency/skills"
)

// fakeRegistry is a skill registry in memory: install writes a SKILL.md
// into the target folder and answers with result (or err).
type fakeRegistry struct {
	name     string
	results  []skillspkg.SearchResult
	err      error
	result   skillspkg.InstallResult
	noWrite  bool // install creates nothing
	searches atomic.Int32
	versions []string
}

func (f *fakeRegistry) Name() string { return f.name }

func (f *fakeRegistry) Search(context.Context, string, int) ([]skillspkg.SearchResult, error) {
	f.searches.Add(1)
	return f.results, f.err
}

func (f *fakeRegistry) GetSkillMeta(context.Context, string) (*skillspkg.SkillMeta, error) {
	return nil, errors.New("not used")
}

func (f *fakeRegistry) DownloadAndInstall(_ context.Context, _, version, targetDir string) (*skillspkg.InstallResult, error) {
	f.versions = append(f.versions, version)
	if !f.noWrite {
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("# skill"), 0o644); err != nil {
			return nil, err
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	r := f.result
	return &r, nil
}

func installTool(t *testing.T, reg *fakeRegistry) (*InstallSkillTool, string) {
	t.Helper()
	ws := t.TempDir()
	mgr := skillspkg.NewRegistryManager()
	mgr.AddRegistry(reg)
	return NewInstallSkillTool(mgr, ws), ws
}

func TestInstall_SuccessWritesOrigin(t *testing.T) {
	reg := &fakeRegistry{name: "alicehub", result: skillspkg.InstallResult{Version: "1.2.0", Summary: "Does things"}}
	tool, ws := installTool(t, reg)
	res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub", "version": "1.2.0"})
	if res.IsError || !strings.Contains(res.ForLLM, `Successfully installed skill "demo" v1.2.0 from alicehub registry.`) ||
		!strings.Contains(res.ForLLM, "Description: Does things") || strings.Contains(res.ForLLM, "suspicious") {
		t.Fatalf("install: %s", res.ForLLM)
	}
	if len(reg.versions) != 1 || reg.versions[0] != "1.2.0" {
		t.Errorf("version asked for: %v", reg.versions)
	}
	data, err := os.ReadFile(filepath.Join(ws, "skills", "demo", ".skill-origin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta originMeta
	if err := json.Unmarshal(data, &meta); err != nil || meta.Registry != "alicehub" || meta.Slug != "demo" || meta.InstalledVersion != "1.2.0" {
		t.Errorf("origin: %s %v", data, err)
	}
}

func TestInstall_SuspiciousWarns(t *testing.T) {
	tool, _ := installTool(t, &fakeRegistry{name: "alicehub", result: skillspkg.InstallResult{Version: "1", IsSuspicious: true}})
	res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub"})
	if res.IsError || !strings.HasPrefix(res.ForLLM, `⚠️ Warning: skill "demo" is flagged as suspicious`) {
		t.Errorf("suspicious: %s", res.ForLLM)
	}
}

// A skill flagged as malware, or one whose install fails, leaves nothing
// behind.
func TestInstall_BlockedOrFailedLeavesNothing(t *testing.T) {
	for name, reg := range map[string]*fakeRegistry{
		"malware": {name: "alicehub", result: skillspkg.InstallResult{Version: "1", IsMalwareBlocked: true}},
		"failure": {name: "alicehub", err: errors.New("download broke")},
	} {
		t.Run(name, func(t *testing.T) {
			tool, ws := installTool(t, reg)
			res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub"})
			want := map[string]string{
				"malware": `skill "demo" is flagged as malicious and cannot be installed`,
				"failure": `failed to install "demo": download broke`,
			}[name]
			if !res.IsError || res.ForLLM != want {
				t.Errorf("result: %s", res.ForLLM)
			}
			if _, err := os.Stat(filepath.Join(ws, "skills", "demo")); !os.IsNotExist(err) {
				t.Errorf("install left behind: %v", err)
			}
		})
	}
}

// A forced reinstall naming a registry that does not exist keeps the
// installed skill.
func TestInstall_ForceWithUnknownRegistryKeepsSkill(t *testing.T) {
	tool, ws := installTool(t, &fakeRegistry{name: "alicehub"})
	skill := filepath.Join(ws, "skills", "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "bobhub", "force": true})
	if !res.IsError || res.ForLLM != `registry "bobhub" not found` {
		t.Errorf("result: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(skill); err != nil || string(got) != "installed" {
		t.Errorf("installed skill lost: %q %v", got, err)
	}
}

func TestInstall_FolderFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	// skills/ is a file: the folder cannot be created.
	tool, ws := installTool(t, &fakeRegistry{name: "alicehub"})
	if err := os.WriteFile(filepath.Join(ws, "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub"})
	if !res.IsError || !strings.HasPrefix(res.ForLLM, "failed to create skills directory: ") {
		t.Errorf("skills is a file: %s", res.ForLLM)
	}

	// The installed skill cannot be removed for a forced reinstall.
	tool, ws = installTool(t, &fakeRegistry{name: "alicehub"})
	skills := filepath.Join(ws, "skills")
	if err := os.MkdirAll(filepath.Join(skills, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "demo", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skills, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skills, 0o700) })
	res = tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub", "force": true})
	if !res.IsError || !strings.HasPrefix(res.ForLLM, `failed to remove existing skill "demo": `) {
		t.Errorf("read-only skills: %s", res.ForLLM)
	}
}

// When the origin record cannot be written the install still succeeds.
func TestInstall_OriginNotWritten(t *testing.T) {
	tool, _ := installTool(t, &fakeRegistry{name: "alicehub", noWrite: true, result: skillspkg.InstallResult{Version: "1"}})
	res := tool.Execute(context.Background(), map[string]any{"slug": "demo", "registry": "alicehub"})
	if res.IsError || !strings.Contains(res.ForLLM, "Successfully installed") {
		t.Errorf("result: %s", res.ForLLM)
	}
}

func TestFind_SearchesCachesAndFails(t *testing.T) {
	reg := &fakeRegistry{name: "alicehub", results: []skillspkg.SearchResult{{Slug: "demo", Score: 0.9, RegistryName: "alicehub", Version: "1"}}}
	mgr := skillspkg.NewRegistryManager()
	mgr.AddRegistry(reg)
	tool := NewFindSkillsTool(mgr, skillspkg.NewSearchCache(10, time.Hour))
	ctx := context.Background()

	res := tool.Execute(ctx, map[string]any{"query": "Demo", "limit": 3.0})
	if res.IsError || !strings.Contains(res.ForLLM, "demo") {
		t.Fatalf("search: %s", res.ForLLM)
	}
	// The same query is answered from the cache.
	res = tool.Execute(ctx, map[string]any{"query": "demo", "limit": 50.0})
	if res.IsError || reg.searches.Load() != 1 {
		t.Errorf("cache: %d searches, %s", reg.searches.Load(), res.ForLLM)
	}

	failing := NewFindSkillsTool(skillspkg.NewRegistryManager(), nil)
	res = failing.Execute(ctx, map[string]any{"query": "demo"})
	if !res.IsError || res.ForLLM != "skill search failed: no registries configured" {
		t.Errorf("no registries: %s", res.ForLLM)
	}
}

func TestGlobalProvider(t *testing.T) {
	if GlobalProvider.Namespace() != "skill" || GlobalProvider.Description() != "Skill discovery and installation" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	c := &config.Config{}
	for _, tc := range []struct {
		cfg any
		ok  bool
	}{{nil, false}, {c, false}} {
		if ok, why := GlobalProvider.Available(tc.cfg); ok != tc.ok || why != "requires_skills" {
			t.Errorf("Available(%v) = %v %q", tc.cfg, ok, why)
		}
	}
	c.Tools.Skills.Registry.Enabled = true
	if ok, why := GlobalProvider.Available(c); !ok || why != "" {
		t.Errorf("Available(enabled) = %v %q", ok, why)
	}

	defs := GlobalProvider.RegisterTools(toolspec.Deps{Cfg: c, Workspace: t.TempDir()})
	byName := map[string]toolspec.ToolDefinition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 2 || byName["find"].Handler == nil || byName["install"].Handler == nil {
		t.Fatalf("definitions: %v", defs)
	}
	// Refusals that need no registry.
	res, err := byName["find"].Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"query": " "}})
	if err != nil || !res.IsError || res.ForLLM != "query is required and must be a non-empty string" {
		t.Errorf("find: %v %+v", err, res)
	}
	res, err = byName["install"].Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"slug": "../escape", "registry": "clawhub"}})
	if err != nil || !res.IsError || !strings.HasPrefix(res.ForLLM, `invalid slug "../escape"`) {
		t.Errorf("install: %v %+v", err, res)
	}
}
