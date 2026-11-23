// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package ccv3

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// skipIfRoot skips a test that relies on file permissions being enforced.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

// chmodRestore sets path's mode and restores restore at cleanup.
func chmodRestore(t *testing.T, path string, mode, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, restore); err != nil {
			t.Error(err)
		}
	})
}

// The exported error kinds read as their sentence and are refusals.
func TestSentinelKinds(t *testing.T) {
	for kind, msg := range map[error]string{
		ErrNotFound:  "card not found",
		ErrExists:    "card already exists",
		ErrInvalidID: "invalid card id",
		ErrTooLarge:  "card too large",
	} {
		if kind.Error() != msg {
			t.Errorf("Error() = %q, want %q", kind.Error(), msg)
		}
		var r interface{ Refusal() bool }
		if !errors.As(kind, &r) || !r.Refusal() {
			t.Errorf("%v is not a refusal", kind)
		}
	}
}

// Open fails when the directory cannot be created, and registers no store.
func TestOpenUncreatableDirectory(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	chmodRestore(t, parent, 0o500, 0o700)
	dir := filepath.Join(parent, "cards")
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "open card store "+dir) {
		t.Fatalf("Open under a read-only folder = %v, want an open card store error", err)
	}
	storesMu.Lock()
	_, ok := stores[dir]
	storesMu.Unlock()
	if ok {
		t.Error("a failed Open registered the store")
	}
}

// List fails when the directory is gone.
func TestListMissingDirectory(t *testing.T) {
	s := newStore(t)
	if err := os.Remove(s.dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.List(); err == nil || !strings.Contains(err.Error(), "list cards") {
		t.Fatalf("List = %v, want a list cards error", err)
	}
}

// A card written by hand without tags lists with an empty tag list.
func TestListCardWithoutTags(t *testing.T) {
	s := newStore(t)
	card := `{"spec":"chara_card_v3","spec_version":"3.0","data":{"name":"Carol"}}`
	if err := os.WriteFile(filepath.Join(s.dir, "carol.json"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	cards, invalid, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 0 || len(cards) != 1 {
		t.Fatalf("List = %+v, %+v", cards, invalid)
	}
	if cards[0].Tags == nil || len(cards[0].Tags) != 0 {
		t.Errorf("Tags = %#v, want an empty list", cards[0].Tags)
	}
}

// Every operation refuses an invalid id before touching the directory, and
// Exists reports false for one.
func TestOperationsRefuseInvalidID(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("bob", json.RawMessage(`{"name":"Bob"}`)); err != nil {
		t.Fatal(err)
	}
	if s.Exists("bad id") {
		t.Error("Exists(bad id) = true")
	}
	_, err := s.Get("bad id")
	wantRefusal(t, err, ErrInvalidID)
	wantRefusal(t, s.Rename("bad id", "carol"), ErrInvalidID)
	wantRefusal(t, s.Copy("bad id", "carol"), ErrInvalidID)
	wantRefusal(t, s.Copy("bob", "bad id"), ErrInvalidID)
	if s.Exists("carol") {
		t.Error("a refused operation wrote a card")
	}
}

// Get reports a read failure that is neither missing nor damaged as an
// error, not a refusal.
func TestGetReadFailure(t *testing.T) {
	s := newStore(t)
	if err := os.Mkdir(s.path("bob"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get("bob")
	if err == nil || !strings.Contains(err.Error(), `read card "bob"`) {
		t.Fatalf("Get(folder) = %v, want a read card error", err)
	}
	var r interface{ Refusal() bool }
	if errors.As(err, &r) {
		t.Errorf("Get(folder) = %v is a refusal", err)
	}
}

// An unreadable card file is an error from readCard.
func TestReadCardUnreadable(t *testing.T) {
	skipIfRoot(t)
	s := newStore(t)
	if _, err := s.Create("bob", json.RawMessage(`{"name":"Bob"}`)); err != nil {
		t.Fatal(err)
	}
	chmodRestore(t, s.path("bob"), 0, 0o600)
	if _, err := readCard(s.path("bob")); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("readCard = %v, want a permission error", err)
	}
}

// Delete, Rename and Import report a failure of the filesystem; the cards
// stay as they were.
func TestWriteFailures(t *testing.T) {
	skipIfRoot(t)
	s := newStore(t)
	if _, err := s.Create("bob", json.RawMessage(`{"name":"Bob"}`)); err != nil {
		t.Fatal(err)
	}
	chmodRestore(t, s.dir, 0o500, 0o700)
	if err := s.Delete("bob"); err == nil || !strings.Contains(err.Error(), `delete card "bob"`) {
		t.Errorf("Delete = %v, want a delete card error", err)
	}
	if err := s.Rename("bob", "carol"); err == nil || !strings.Contains(err.Error(), `rename card "bob" to "carol"`) {
		t.Errorf("Rename = %v, want a rename card error", err)
	}
	if _, _, err := s.Import("carol", []byte(`{"spec":"chara_card_v3","data":{"name":"Carol"}}`), false); err == nil ||
		!strings.Contains(err.Error(), `write card "carol"`) {
		t.Errorf("Import = %v, want a write card error", err)
	}
	if !s.Exists("bob") || s.Exists("carol") {
		t.Error("a failed delete, rename or import changed the cards")
	}
}

// A spec that is not text is refused.
func TestImportSpecNotText(t *testing.T) {
	s := newStore(t)
	_, _, err := s.Import("bob", []byte(`{"spec":3,"data":{"name":"Bob"}}`), false)
	if err == nil || err.Error() != "The card's spec must be text." {
		t.Fatalf("Import = %v", err)
	}
}

// Lorebook entries that are not a list, or entries that are not objects,
// are left for Validate to report rather than patched.
func TestNormalizeImportLeavesMalformedEntries(t *testing.T) {
	for _, tc := range []struct {
		name, data string
	}{
		{"entries not a list", `{"name":"B","character_book":{"entries":"x"}}`},
		{"entry not an object", `{"name":"B","character_book":{"entries":[1]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := normalizeImport(json.RawMessage(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.data {
				t.Errorf("normalizeImport = %s, want %s unchanged", out, tc.data)
			}
			if Validate(out, true) == nil {
				t.Error("Validate accepted malformed entries")
			}
		})
	}
}

// array reports false for text that starts like an array but is not JSON.
func TestArrayMalformed(t *testing.T) {
	if _, ok := array(json.RawMessage(`[1,`)); ok {
		t.Error("array accepted malformed JSON")
	}
}

// allowNull adds null to a type list and to an enum, once.
func TestAllowNull(t *testing.T) {
	s := map[string]any{"type": []string{"integer", "string"}, "enum": []any{"a"}}
	allowNull(s)
	if typ, _ := s["type"].([]string); !slices.Equal(typ, []string{"integer", "string", "null"}) {
		t.Errorf("type = %v", typ)
	}
	if enum, _ := s["enum"].([]any); len(enum) != 2 || enum[1] != nil {
		t.Errorf("enum = %v", enum)
	}
	s = map[string]any{"type": []string{"string", "null"}}
	allowNull(s)
	if typ, _ := s["type"].([]string); !slices.Equal(typ, []string{"string", "null"}) {
		t.Errorf("type = %v, want null once", typ)
	}
}
