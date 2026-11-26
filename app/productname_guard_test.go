// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package app

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// oldName matches the product's old name ("claw", "ClawEh") anywhere in a
// word, in any case: CLAW_HOME, mcp__claw__ and claw.log are all old names.
var oldName = regexp.MustCompile(`(?i)claw`)

// thirdPartyNames are names of other people's things that contain the old
// name. Each is removed from a line before oldName is checked. To allow
// another, add the narrowest pattern that matches only it, with a comment
// saying what it names.
var thirdPartyNames = []*regexp.Regexp{
	// The OpenClaw device protocol, its Go module (a3tai/openclaw-go) and the
	// openclaw link the Rabbit R1 starts.
	regexp.MustCompile(`(?i)openclaw`),
	// PicoClaw (sipeed/picoclaw), the project Agency was forked from.
	regexp.MustCompile(`(?i)picoclaw`),
	// The ClawHub skills registry (clawhub.ai) and its config key.
	regexp.MustCompile(`(?i)clawhub`),
	// The setup payload type current R1 firmware parses.
	regexp.MustCompile(`clawdbot-gateway`),
	// The "Claw to Talk" Android app (com.alvin.clawtotalk) and the session
	// key segment it sends.
	regexp.MustCompile(`Claw to Talk|clawtotalk`),
}

// skipFiles are files (or folders, ending in /) that are not checked, each
// with the reason.
var skipFiles = map[string]string{
	"app/productname_guard_test.go":               "this guard names what it looks for",
	"web/frontend/src/product-name-guard.test.ts": "the frontend guard, likewise",
	"channels/device/assets/bip39-english.txt":    `the BIP39 word list; "claw" is one of its words`,
	"refactor/": "the refactor's history, written under the old name",
}

// oldNameMarker marks a line of a test that checks an old name is ignored.
const oldNameMarker = "rename-guard:old-name"

// allowedSections are the parts of a file where the old name is history or
// is what the reader has to type: from a line matching start up to the next
// line matching end (nil: the end of the file).
var allowedSections = map[string]struct{ start, end *regexp.Regexp }{
	// Every release section but the topmost, which is still in development.
	"CHANGELOG.md": {start: regexp.MustCompile(`^## \[`)},
}

// TestOldName_Nowhere enforces that the product's old name appears nowhere
// in the tree: not in code, tests, scripts, docs or file names, except for
// third-party names, the files in skipFiles, the sections in
// allowedSections and lines marked with oldNameMarker.
func TestOldName_Nowhere(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files := trackedFiles(t, root)
	if len(files) < 100 {
		t.Fatalf("found only %d files; the file listing is broken", len(files))
	}
	var hits []string
	for _, rel := range files {
		if skipped(rel) {
			continue
		}
		if namesOldName(rel) {
			hits = append(hits, rel+": file name")
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the work tree, not yet in the index
			}
			t.Fatal(err)
		}
		hits = append(hits, oldNameLines(rel, data)...)
	}
	for _, h := range hits {
		t.Error(h)
	}
	if len(hits) > 0 {
		t.Log("The product's old name is gone. Use the current names (global/identifiers.go); " +
			"a third-party name belongs in thirdPartyNames.")
	}
}

// trackedFiles lists the files git tracks under root, as slash paths from it;
// without git it walks the tree, leaving out dot folders and build output.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	}
	generated := map[string]bool{
		"build": true, "bin": true, "node_modules": true,
		"web/backend/dist": true, "web/backend/manual": true,
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || generated[rel] || generated[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func skipped(rel string) bool {
	for p := range skipFiles {
		if rel == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			return true
		}
	}
	return false
}

// oldNameLines returns "file:line: text" for each line of a text file that
// names the old name outside the allowed parts. Binary files are skipped.
func oldNameLines(rel string, data []byte) []string {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	sec, hasSection := allowedSections[rel]
	inSection, starts := false, 0
	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		if hasSection {
			switch {
			case inSection && sec.end != nil && sec.end.MatchString(line):
				inSection = false
			case !inSection && sec.start.MatchString(line):
				starts++
				// The CHANGELOG's first release heading is the one in
				// development; the allowed history starts at the second.
				inSection = rel != "CHANGELOG.md" || starts > 1
			}
		}
		if inSection || strings.Contains(line, oldNameMarker) {
			continue
		}
		if namesOldName(line) {
			out = append(out, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return out
}

// namesOldName reports whether s holds the old name once the third-party
// names are taken out.
func namesOldName(s string) bool {
	for _, re := range thirdPartyNames {
		s = re.ReplaceAllString(s, " ")
	}
	return oldName.MatchString(s)
}

// TestNamesOldName pins the guard's matching, so a loosened allow-list or
// pattern shows up here rather than as a silent gap.
func TestNamesOldName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"ClawEh", true},
		{"Claw is shutting down", true},
		{"run: claw admin", true},
		{"CLAW_HOME", true},
		{"mcp__claw__*", true},
		{"~/.claw/config.json", true},
		{"/opt/claw", true},
		{"logs/claw.log", true},
		{"claw-auth version %s", true},
		{"agency_home", false},
		{"openclaw", false},
		{"OpenClaw Gateway", false},
		{"github.com/a3tai/openclaw-go", false},
		{"sipeed/picoclaw", false},
		{"clawhub.ai", false},
		{"AGENCY_SKILLS_REGISTRIES_CLAWHUB_ENABLED", false},
		{"clawdbot-gateway", false},
		{"com.alvin.clawtotalk", false},
		{"the Claw to Talk app", false},
		{"openclaw acp; claw acp", true},
	}
	for _, tt := range tests {
		if got := namesOldName(tt.in); got != tt.want {
			t.Errorf("namesOldName(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestOldNameSections pins the allowed sections: released CHANGELOG history
// is allowed, the release in development and the manual are not, and a
// marked line is allowed anywhere.
func TestOldNameSections(t *testing.T) {
	changelog := "# Changelog\n## [0.6.6]\n- claw in development\n## [0.6.5]\n- claw released\n## [0.6.4]\n- claw older\n"
	if got := oldNameLines("CHANGELOG.md", []byte(changelog)); len(got) != 1 || !strings.Contains(got[0], "CHANGELOG.md:3:") {
		t.Errorf("CHANGELOG hits = %q, want only line 3", got)
	}
	manual := "# Upgrading\nclaw\n## After\nclaw\n"
	if got := oldNameLines("manual/getting-started/upgrading.md", []byte(manual)); len(got) != 2 {
		t.Errorf("manual hits = %q, want both lines", got)
	}
	if got := oldNameLines("x_test.go", []byte("a := \"claw\" // "+oldNameMarker+"\nb := \"claw\"\n")); len(got) != 1 {
		t.Errorf("marked hits = %q, want only the unmarked line", got)
	}
}
