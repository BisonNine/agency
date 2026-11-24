// Agency
// License: MIT

package servicetoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "servicetoken: read") {
		t.Errorf("directory path err = %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "servicetoken: parse") {
		t.Errorf("corrupt file err = %v", err)
	}
}

func TestLoad_NullFileIsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(p, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Load(null) = %#v, want an empty non-nil map", got)
	}
}

// A plaintext file that cannot be rewritten in hashed form is an error, so
// plaintext tokens are never served from a file that stays plaintext.
func TestLoad_MigrationWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "internal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, fileName)
	if err := os.WriteFile(p, []byte(`{"alice":"SSTplaintext"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "rewrite") {
		t.Fatalf("err = %v, want rewrite failure", err)
	}
}

func TestSave_Errors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(blocker, "internal", fileName), map[string]string{}); err == nil || !strings.Contains(err.Error(), "servicetoken: mkdir") {
		t.Errorf("parent under a file err = %v", err)
	}
	target := filepath.Join(t.TempDir(), "tokens")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(target, map[string]string{}); err == nil || !strings.Contains(err.Error(), "servicetoken: write") {
		t.Errorf("target is a dir err = %v", err)
	}
}

func TestPath(t *testing.T) {
	if got, want := Path("/data"), filepath.Join("/data", "internal", fileName); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
