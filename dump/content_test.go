package dump

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrite_NoDirIsNoop(t *testing.T) {
	name, err := Write(Dir{}, "r", nil, nil, nil)
	if name != "" || err != nil {
		t.Fatalf("Write with no dir = %q, %v; want empty, nil", name, err)
	}
}

func TestWrite_Contents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	meta := map[string]any{"model": "gpt", "agent": "alice", "extra": 7}
	input := json.RawMessage(`{"prompt":"line1\nline2","html":"<b>"}`)
	output := json.RawMessage(`"done"`)
	base, err := Write(Dir{Path: dir}, "empty_response", meta, input, output)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, base+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata map[string]any  `json:"metadata"`
		Input    json.RawMessage `json:"input"`
		Output   json.RawMessage `json:"output"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json dump is not JSON: %v", err)
	}
	if doc.Metadata["reason"] != "empty_response" || doc.Metadata["agent"] != "alice" {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	if !strings.Contains(string(raw), "<b>") {
		t.Error("HTML is escaped in the json dump")
	}

	txt, err := os.ReadFile(filepath.Join(dir, base+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(txt)
	// Known keys come first in a fixed order, then the rest.
	wantHead := "REASON: empty_response\nagent: alice\nmodel: gpt\nextra: 7\n"
	if !strings.HasPrefix(s, wantHead) {
		t.Errorf("txt head = %q, want prefix %q", s, wantHead)
	}
	if !strings.Contains(s, "line1\nline2") {
		t.Error("escaped newlines not expanded in the txt dump")
	}
	if !strings.Contains(s, "\n-----\n\nOUTPUT\n\n-----\n\n\"done\"\n") {
		t.Errorf("output section missing: %q", s)
	}
}

func TestPrettyJSON_NotJSON(t *testing.T) {
	if got := prettyJSON(json.RawMessage("not json")); got != "not json" {
		t.Errorf("prettyJSON = %q, want the raw text", got)
	}
}

func TestWrite_Errors(t *testing.T) {
	// Invalid raw JSON cannot be encoded.
	if _, err := Write(Dir{Path: t.TempDir()}, "r", nil, json.RawMessage("{"), nil); err == nil || !strings.Contains(err.Error(), "dump: marshal json") {
		t.Errorf("invalid input err = %v", err)
	}

	if os.Geteuid() == 0 {
		return // root ignores directory permissions
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ro, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := Write(Dir{Path: filepath.Join(ro, "dumps")}, "r", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "dump: create dir") {
		t.Errorf("unwritable parent err = %v", err)
	}
	if _, err := Write(Dir{Path: ro}, "r", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "dump: write json") {
		t.Errorf("read-only dumps dir err = %v", err)
	}
}
