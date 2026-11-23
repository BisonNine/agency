// ClawEh
// License: MIT

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/skills"
)

// A skill filter pattern matches by '*' as a wildcard; the text on either
// side of the '*' must both fit in the name without overlapping.
func TestCtxCov_MatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"fetch", "fetch", true},
		{"fetch", "fetcher", false},
		{"fetch-*", "fetch-web", true},
		{"fetch-*", "get-web", false},
		{"*-web", "fetch-web", true},
		{"*-web", "fetch-mail", false},
		{"a*z", "abcz", true},
		{"a*z", "az", true},
		{"aba*aba", "aba", false},
		{"ab*ba", "aba", false},
		{"ab*ba", "abba", true},
		// Every star is a wildcard.
		{"a*b*c", "abc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "acb", false},
		{"a*b*c", "abcx", false},
		{"*mail*", "send-mail-now", true},
		{"*mail*", "post", false},
		{"**", "", true},
		{"a**z", "az", true},
		{"*a*a*", "a", false},
		{"*a*a*", "aa", true},
		{"x*y*z", "xyz", true},
		{"x*yz*z", "xyz", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// A fixed prompt is the whole system prompt.
func TestCtxCov_FixedPrompt(t *testing.T) {
	cb := NewContextBuilder(t.TempDir()).WithFixedPrompt("You are Bob.")
	if got := cb.BuildSystemPrompt(); got != "You are Bob." {
		t.Fatalf("BuildSystemPrompt = %q, want the fixed prompt", got)
	}
}

// A character card that cannot be rendered leaves it out of the prompt and
// logs a warning; the agent runs without it.
func TestCtxCov_CardRenderFails(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	ws := t.TempDir()
	card := filepath.Join(t.TempDir(), "alice.json")
	cb := NewContextBuilder(ws).WithCharacterCard(card, func() (string, error) {
		return "", errors.New("card is not valid JSON")
	})

	if got := cb.cardSection(); got != "" {
		t.Fatalf("cardSection = %q, want empty", got)
	}
	if !strings.Contains(buf.String(), "Character card not rendered") {
		t.Fatalf("no warning logged:\n%s", buf.String())
	}
}

// The global config dir is CLAW_HOME, else ~/.claw, else "" when there is
// no home directory.
func TestCtxCov_GlobalConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, "")
	t.Setenv("HOME", home)
	if got := getGlobalConfigDir(); got != filepath.Join(home, ".claw") {
		t.Fatalf("getGlobalConfigDir = %q, want %s", got, filepath.Join(home, ".claw"))
	}
	t.Setenv("HOME", "")
	if got := getGlobalConfigDir(); got != "" {
		t.Fatalf("getGlobalConfigDir without a home = %q, want empty", got)
	}
}

// Without a skills loader, or one with no roots, the workspace's skills
// folder is the one skill root.
func TestCtxCov_SkillRootsFallback(t *testing.T) {
	ws := t.TempDir()
	want := []string{filepath.Join(ws, "skills")}
	if got := (&ContextBuilder{workspace: ws}).skillRoots(); !slices.Equal(got, want) {
		t.Errorf("no loader: %v, want %v", got, want)
	}
	cb := &ContextBuilder{workspace: ws, skillsLoader: &skills.SkillsLoader{}}
	if got := cb.skillRoots(); !slices.Equal(got, want) {
		t.Errorf("no roots: %v, want %v", got, want)
	}
}

// An empty workspace gives the cache a very old, non-zero baseline, so any
// file created later counts as a change.
func TestCtxCov_CacheBaselineEmptyWorkspace(t *testing.T) {
	cb := &ContextBuilder{workspace: filepath.Join(t.TempDir(), "absent")}
	b := cb.buildCacheBaseline()
	if !b.maxMtime.Equal(time.Unix(1, 0)) {
		t.Fatalf("maxMtime = %v, want %v", b.maxMtime, time.Unix(1, 0))
	}
	if len(b.skillFiles) != 0 {
		t.Fatalf("skillFiles = %v, want none", b.skillFiles)
	}
}

// A builder whose cache was never built reports its sources changed.
func TestCtxCov_NoCacheMeansChanged(t *testing.T) {
	cb := &ContextBuilder{workspace: t.TempDir()}
	if !cb.sourceFilesChangedLocked() {
		t.Error("sourceFilesChangedLocked = false with no cache")
	}
	if !cb.fileChangedSince(filepath.Join(cb.workspace, "AGENTS.md")) {
		t.Error("fileChangedSince = false with no snapshot")
	}
	if !skillFilesChangedSince(nil, nil) {
		t.Error("skillFilesChangedSince = false with no snapshot")
	}
}

// The skill tree is compared with its snapshot: blank roots are ignored, a
// new file is a change, and a folder that cannot be read is treated as a
// change rather than trusted.
func TestCtxCov_SkillFilesChangedSince(t *testing.T) {
	root := t.TempDir()
	if skillFilesChangedSince([]string{" "}, map[string]time.Time{}) {
		t.Error("a blank root counted as a change")
	}
	if skillFilesChangedSince([]string{root}, map[string]time.Time{}) {
		t.Error("an empty root counted as a change")
	}

	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !skillFilesChangedSince([]string{root}, map[string]time.Time{}) {
		t.Error("a new skill file was not a change")
	}

	locked := t.TempDir()
	sub := filepath.Join(locked, "skill")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sub, 0o700); err != nil {
			t.Errorf("restore %s: %v", sub, err)
		}
	})
	if !skillFilesChangedSince([]string{locked}, map[string]time.Time{}) {
		t.Error("an unreadable skill folder was not a change")
	}
}
