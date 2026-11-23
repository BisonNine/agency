// ClawEh
// License: MIT

package msgtoken

import (
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agency/internal/testalerts"
)

// fileBlocker returns a path whose parent is a regular file.
func fileBlocker(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f, "tokens.json")
}

// nonEmptyDir returns a path that is a non-empty directory, so a file cannot
// be renamed over it.
func nonEmptyDir(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewManager_DisabledRemovalFails(t *testing.T) {
	p := nonEmptyDir(t)
	m, err := NewManager("alice", p, 0, 0)
	if m != nil || err != nil {
		t.Fatalf("disabled = %v, %v; want nil, nil", m, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("path vanished: %v", err)
	}
}

func TestNewManager_SaveFailures(t *testing.T) {
	if _, err := NewManager("alice", fileBlocker(t), 60, 2); err == nil {
		t.Error("store dir cannot be made: want error")
	}
	rec := testalerts.Install(t)
	if _, err := NewManager("alice", nonEmptyDir(t), 60, 2); err == nil {
		t.Error("store cannot be written: want error")
	}
	got := rec.Alerts()
	if len(got) != 1 || got[0].Title != "Message-token store not written" || got[0].EventID != "msgtoken:alice" {
		t.Errorf("alerts = %+v", got)
	}
}

func TestCurrentToken_Empty(t *testing.T) {
	if got := (&Manager{}).CurrentToken(); got != "" {
		t.Errorf("CurrentToken with no tokens = %q", got)
	}
}

func TestNewNamedStore_ReadErrorAndNull(t *testing.T) {
	if _, err := NewNamedStore(t.TempDir()); err == nil {
		t.Error("store path is a directory: want error")
	}
	p := filepath.Join(t.TempDir(), "named.json")
	if err := os.WriteFile(p, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewNamedStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("alice", "phone"); err != nil {
		t.Fatalf("Create on a store loaded from null: %v", err)
	}
}

// A token that cannot be saved is not kept in memory either.
func TestNamedStore_CreateRollsBackOnSaveFailure(t *testing.T) {
	for name, path := range map[string]string{"mkdir": fileBlocker(t), "write": nonEmptyDir(t)} {
		t.Run(name, func(t *testing.T) {
			s := &NamedStore{path: path, tokens: map[string][]NamedToken{}, limits: map[string]*tokenLimit{}}
			if _, err := s.Create("alice", "phone"); err == nil {
				t.Fatal("Create: want error")
			}
			if n := len(s.List("alice")); n != 0 {
				t.Errorf("%d tokens kept after a failed save", n)
			}
		})
	}
}

func TestNamedStore_UpdateNotPersistedAlerts(t *testing.T) {
	rec := testalerts.Install(t)
	s, err := NewNamedStore(filepath.Join(t.TempDir(), "named.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Create("alice", "phone")
	if err != nil {
		t.Fatal(err)
	}
	s.path = fileBlocker(t)
	if !s.Update("alice", tok.ID, 10, 5) {
		t.Fatal("Update must still apply in memory")
	}
	if got := s.List("alice")[0]; got.RatePerMin != 10 || got.BlockMinutes != 5 {
		t.Errorf("token = %+v", got)
	}
	if got := rec.Alerts(); len(got) != 1 || got[0].Title != "Message-token store not written" {
		t.Errorf("alerts = %+v", got)
	}
}

func TestNamedStore_ValidateEmptyAndLimitDefaults(t *testing.T) {
	s, err := NewNamedStore("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("alice", "phone"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.ValidateWithID(""); ok {
		t.Error("empty token validated")
	}
	rate, block := s.tokenLimitConfig("alice", "missing")
	if rate != DefaultRatePerMin || block != DefaultBlockMinutes {
		t.Errorf("missing token limits = %d/%d, want defaults", rate, block)
	}
}
