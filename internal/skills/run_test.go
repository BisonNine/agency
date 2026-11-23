package skills

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	return string(<-done)
}

// setupHome points the config at a fresh temp data directory and returns it.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAW_HOME", home)
	return home
}

// runSkills runs the skills command with args and returns its stdout and error.
func runSkills(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := NewSkillsCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return out, err
}

func writeSkill(t *testing.T, home, dir, body string) {
	t.Helper()
	p := filepath.Join(home, "skills", dir)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(body), 0o600))
}

func testZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// clawhub serves a fake ClawHub registry and enables it through the config
// environment.
func clawhub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("CLAW_SKILLS_REGISTRIES_CLAWHUB_ENABLED", "true")
	t.Setenv("CLAW_SKILLS_REGISTRIES_CLAWHUB_BASE_URL", srv.URL)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(v))
}

func TestSkillsRootRunShowsHelp(t *testing.T) {
	setupHome(t)
	cmd := NewSkillsCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "Manage skills")
}

func TestSkillsPreRunErrors(t *testing.T) {
	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600))
		_, err := runSkills(t, "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "error loading config")
	})

	t.Run("bad proxy", func(t *testing.T) {
		setupHome(t)
		t.Setenv("CLAW_TOOLS_SKILLS_GITHUB_PROXY", "ftp://proxy.invalid")
		_, err := runSkills(t, "list")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "error creating skills installer")
	})
}

// Subcommands run without the root's pre-run have no installer or loader.
func TestSkillsSubcommandsUninitialized(t *testing.T) {
	root := NewSkillsCommand()
	tests := map[string]string{
		"list":    "skills loader is not initialized",
		"show":    "skills loader is not initialized",
		"install": "skills installer is not initialized",
		"remove":  "skills installer is not initialized",
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			sub, _, err := root.Find([]string{name})
			require.NoError(t, err)
			err = sub.RunE(sub, []string{"x"})
			require.Error(t, err)
			assert.Equal(t, want, err.Error())
		})
	}
}

func TestSkillsList(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		setupHome(t)
		out, err := runSkills(t, "list")
		require.NoError(t, err)
		assert.Equal(t, "No skills installed.\n", out)
	})

	t.Run("installed", func(t *testing.T) {
		home := setupHome(t)
		writeSkill(t, home, "alice", "---\nname: alice\ndescription: Says hello\n---\n# Alice\n")
		writeSkill(t, home, "bob", "---\nname: bob\ndescription: Waves\n---\n")
		out, err := runSkills(t, "list")
		require.NoError(t, err)
		assert.Contains(t, out, "\nInstalled Skills:\n------------------\n")
		assert.Contains(t, out, "  ✓ alice (global)\n    Says hello\n")
		assert.Contains(t, out, "  ✓ bob (global)\n    Waves\n")
	})
}

func TestSkillsShow(t *testing.T) {
	home := setupHome(t)
	writeSkill(t, home, "alice", "---\nname: alice\ndescription: d\n---\nBody text\n")

	out, err := runSkills(t, "show", "alice")
	require.NoError(t, err)
	assert.Contains(t, out, "\n📦 Skill: alice\n----------------------\n")
	assert.Contains(t, out, "Body text")
	assert.NotContains(t, out, "description: d")

	out, err = runSkills(t, "show", "bob")
	require.NoError(t, err)
	assert.Equal(t, "✗ Skill 'bob' not found\n", out)
}

func TestSkillsRemove(t *testing.T) {
	home := setupHome(t)
	writeSkill(t, home, "alice", "---\nname: alice\n---\n")

	out, err := runSkills(t, "remove", "alice")
	require.NoError(t, err)
	assert.Equal(t, "Removing skill 'alice'...\n✓ Skill 'alice' removed successfully!\n", out)
	assert.NoDirExists(t, filepath.Join(home, "skills", "alice"))
}

// A failed removal exits the process with status 1, so it runs in a child.
func TestSkillsRemoveMissingExits(t *testing.T) {
	if os.Getenv("SKILLS_REMOVE_CHILD") == "1" {
		cmd := NewSkillsCommand()
		cmd.SetArgs([]string{"remove", "bob"})
		_ = cmd.Execute()
		return
	}
	home := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestSkillsRemoveMissingExits$")
	child.Env = append(os.Environ(), "SKILLS_REMOVE_CHILD=1", "CLAW_HOME="+home)
	out, err := child.Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(out), "✗ Failed to remove skill: skill 'bob' not found")
}

func TestSkillsInstallFromGitHubErrors(t *testing.T) {
	home := setupHome(t)
	writeSkill(t, home, "weather", "---\nname: weather\n---\n")

	out, err := runSkills(t, "install", "alice/tools/weather")
	require.Error(t, err)
	assert.Equal(t, "failed to install skill: skill 'weather' already exists", err.Error())
	assert.Equal(t, "Installing skill from alice/tools/weather...\n", out)

	_, err = runSkills(t, "install", "noslash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected 'owner/repo'")
}

func TestSkillsInstallFromRegistry(t *testing.T) {
	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t)
		cmd := NewSkillsCommand()
		require.NoError(t, cmd.PersistentPreRunE(cmd, nil))
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
		install, _, err := cmd.Find([]string{"install"})
		require.NoError(t, err)
		require.NoError(t, install.Flags().Set("registry", "clawhub"))
		require.Error(t, install.RunE(install, []string{"alice"}))
	})

	t.Run("invalid registry name", func(t *testing.T) {
		setupHome(t)
		_, err := runSkills(t, "install", "--registry", "../x", "alice")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "✗  invalid registry name:")
	})

	t.Run("invalid slug", func(t *testing.T) {
		setupHome(t)
		_, err := runSkills(t, "install", "--registry", "clawhub", "../alice")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "✗  invalid slug:")
	})

	t.Run("registry not enabled", func(t *testing.T) {
		setupHome(t)
		_, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.Error(t, err)
		assert.Equal(t, "✗  registry 'clawhub' not found or not enabled. check your config.json.", err.Error())
	})

	t.Run("already installed", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		writeSkill(t, home, "alice", "x")
		_, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.Error(t, err)
		assert.Equal(t, "✗ skill 'alice' already installed at "+filepath.Join(home, "skills", "alice"), err.Error())
	})

	t.Run("skills directory cannot be created", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		// Seed the config first, then block the skills directory with a file.
		_, err := runSkills(t, "list")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, "skills"), nil, 0o600))
		_, err = runSkills(t, "install", "--registry", "clawhub", "alice")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "✗ failed to create skills directory:")
	})

	t.Run("download fails", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		out, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "✗ failed to install skill: download failed: HTTP 404")
		assert.Equal(t, "Installing skill 'alice' from clawhub registry...\n", out)
		assert.NoDirExists(t, filepath.Join(home, "skills", "alice"))
	})

	zipBody := testZip(t, map[string]string{"SKILL.md": "---\nname: alice\ndescription: d\n---\nHi\n"})
	registry := func(t *testing.T, moderation map[string]bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/skills/alice":
				writeJSON(t, w, map[string]any{
					"slug":          "alice",
					"summary":       "Greets people",
					"latestVersion": map[string]string{"version": "1.2.3"},
					"moderation":    moderation,
				})
			case "/api/v1/download":
				assert.Equal(t, "1.2.3", r.URL.Query().Get("version"))
				_, _ = w.Write(zipBody)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}

	t.Run("installs", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, registry(t, nil))
		out, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.NoError(t, err)
		assert.Equal(t, "Installing skill 'alice' from clawhub registry...\n"+
			"✓ Skill 'alice' v1.2.3 installed successfully!\n  Greets people\n", out)
		assert.FileExists(t, filepath.Join(home, "skills", "alice", "SKILL.md"))
	})

	t.Run("suspicious installs with a warning", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, registry(t, map[string]bool{"isSuspicious": true}))
		out, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.NoError(t, err)
		assert.Contains(t, out, "⚠️  Warning: skill 'alice' is flagged as suspicious.\n")
		assert.Contains(t, out, "✓ Skill 'alice' v1.2.3 installed successfully!")
		assert.FileExists(t, filepath.Join(home, "skills", "alice", "SKILL.md"))
	})

	t.Run("malware is removed", func(t *testing.T) {
		home := setupHome(t)
		clawhub(t, registry(t, map[string]bool{"isMalwareBlocked": true}))
		_, err := runSkills(t, "install", "--registry", "clawhub", "alice")
		require.Error(t, err)
		assert.Equal(t, "✗ Skill 'alice' is flagged as malicious and cannot be installed.\n", err.Error())
		assert.NoDirExists(t, filepath.Join(home, "skills", "alice"))
	})
}

func TestSkillsSearch(t *testing.T) {
	t.Run("bad config", func(t *testing.T) {
		home := setupHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{bad"), 0o600))
		out := captureStdout(t, func() { skillsSearchCmd("") })
		assert.Contains(t, out, "Searching for available skills...\n✗ Failed to load config:")
	})

	t.Run("no registries", func(t *testing.T) {
		setupHome(t)
		out, err := runSkills(t, "search")
		require.NoError(t, err)
		assert.Equal(t, "Searching for available skills...\n✗ Failed to fetch skills list: no registries configured\n", out)
	})

	t.Run("no results", func(t *testing.T) {
		setupHome(t)
		clawhub(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"results": []any{}})
		})
		out, err := runSkills(t, "search", "nothing")
		require.NoError(t, err)
		assert.Equal(t, "Searching for available skills...\nNo skills available.\n", out)
	})

	t.Run("results", func(t *testing.T) {
		setupHome(t)
		clawhub(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/search", r.URL.Path)
			assert.Equal(t, "greet", r.URL.Query().Get("q"))
			assert.Equal(t, "20", r.URL.Query().Get("limit"))
			writeJSON(t, w, map[string]any{"results": []map[string]any{
				{"slug": "alice", "displayName": "Alice", "summary": "Greets", "version": "1.0.0", "score": 2},
				{"slug": "bob", "summary": "Waves", "score": 1},
			}})
		})
		out, err := runSkills(t, "search", "greet")
		require.NoError(t, err)
		assert.Contains(t, out, "\nAvailable Skills (2):\n--------------------\n")
		assert.Contains(t, out, "  📦 Alice\n     Greets\n     Slug: alice\n     Registry: clawhub\n     Version: 1.0.0\n\n")
		assert.Contains(t, out, "  📦 bob\n     Waves\n     Slug: bob\n     Registry: clawhub\n\n")
	})
}
