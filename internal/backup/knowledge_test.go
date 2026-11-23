package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// knowledgeTree writes knowledge bases for alice (switch on) and bob (switch
// off in the config: his bundles must still be kept), plus what is not
// archived: kb's temporary file of a write in progress and symbolic links.
// It returns the files that must be archived, relative to the agents folder.
func knowledgeTree(t *testing.T, src Source) map[string]maestroFile {
	t.Helper()
	writeFileT(t, src.ConfigPath, `{"agents":{"list":[`+
		`{"id":"alice","knowledge":{"enabled":true}},{"id":"bob","knowledge":{"enabled":false}}]}}`, 0o600)
	keep := map[string]maestroFile{
		"alice/okf/notes/guide/crypto.md":   {"---\ntype: Note\n---\nKey sizes.\n", 0o640},
		"alice/okf/notes/SFR/FCS_COP.1.md":  {"---\ntype: SFR\n---\nEncrypt.\n", 0o600},
		"alice/okf/notes/schemas/note.json": {`{"type":"object"}`, 0o644},
		"alice/okf/audit/index.md":          {"# Audit\n", 0o600},
		"alice/okf/audit/cache.db":          {"not a database", 0o600},
		"bob/okf/notes/a.md":                {"---\ntype: Note\n---\nBob's note.\n", 0o600},
	}
	for rel, f := range keep {
		writeFileT(t, filepath.Join(src.AgentsDir, filepath.FromSlash(rel)), f.content, f.mode)
	}
	okf := filepath.Join(src.AgentsDir, "alice", "okf")
	writeFileT(t, filepath.Join(okf, "notes", "guide", ".crypto.md.0123456789abcdef.tmp"), "half", 0o600)
	secret := filepath.Join(t.TempDir(), "secret")
	writeFileT(t, filepath.Join(secret, "key.md"), "SECRET", 0o600)
	if err := os.Symlink(filepath.Join(secret, "key.md"), filepath.Join(okf, "notes", "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(okf, "linkedbundle")); err != nil {
		t.Fatal(err)
	}
	// An okf folder that is itself a link is not followed.
	if err := os.MkdirAll(filepath.Join(src.AgentsDir, "carol"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src.AgentsDir, "carol", "okf")); err != nil {
		t.Fatal(err)
	}
	return keep
}

func TestRunArchivesKnowledgeBases(t *testing.T) {
	src := fixture(t)
	keep := knowledgeTree(t, src)
	res, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, entries := readTar(t, res.Archive)
	for rel, f := range keep {
		e, ok := entries["agents/"+rel]
		if !ok {
			t.Errorf("agents/%s missing from the archive", rel)
			continue
		}
		if string(e.data) != f.content || os.FileMode(e.hdr.Mode) != f.mode {
			t.Errorf("agents/%s = %q mode %o, want %q mode %o", rel, e.data, e.hdr.Mode, f.content, f.mode)
		}
	}
	for name, e := range entries {
		if strings.Contains(name, "/okf/") &&
			(strings.HasSuffix(name, ".tmp") || strings.Contains(name, "linked") || strings.HasPrefix(name, "agents/carol/")) {
			t.Errorf("%s must not be archived", name)
		}
		if strings.Contains(string(e.data), "SECRET") {
			t.Errorf("%s holds a link target's content", name)
		}
	}
}

func TestRestoreKnowledgeBases(t *testing.T) {
	src := fixture(t)
	keep := knowledgeTree(t, src)
	res, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	plan, err := PlanRestore(res.Archive, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for rel, f := range keep {
		path := filepath.Join(target, "agents", filepath.FromSlash(rel))
		got, err := os.ReadFile(path)
		if err != nil || string(got) != f.content {
			t.Errorf("restored %s = %q err=%v, want %q", rel, got, err, f.content)
			continue
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("restored %s err=%v, want mode %o", rel, err, f.mode)
		}
	}
	for _, absent := range []string{
		"alice/okf/notes/guide/.crypto.md.0123456789abcdef.tmp",
		"alice/okf/notes/linked.md",
		"alice/okf/linkedbundle",
		"carol/okf",
	} {
		if _, err := os.Lstat(filepath.Join(target, "agents", filepath.FromSlash(absent))); !os.IsNotExist(err) {
			t.Errorf("%s must not be restored (err=%v)", absent, err)
		}
	}
}
