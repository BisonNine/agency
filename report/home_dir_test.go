// Agency
// License: MIT

package report

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agency/config"
)

// TestHomeDirRow: the row is shown only when the home is the data directory
// or inside it, by path component and with links resolved.
func TestHomeDirRow(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "agency")
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "home-link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	want := "Home directory is inside the data directory (" + data + "); CLI providers may not run."

	for _, tc := range []struct {
		name, home, data string
		shown            bool
	}{
		{"equal", data, data, true},
		{"equal with trailing slash", data + "/", data, true},
		{"descendant", filepath.Join(data, "sub"), data, true},
		{"descendant that does not exist", filepath.Join(data, "none", "x"), data, true},
		{"symlink into the data directory", link, data, true},
		{"sibling with a shared prefix", data + "2", data, false},
		{"parent", base, data, false},
		{"unrelated", "/home/alice", data, false},
		{"no home", "", data, false},
		{"no data directory", data, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := homeDirRow(tc.home, tc.data)
			if ok != tc.shown {
				t.Fatalf("shown = %v, want %v (%+v)", ok, tc.shown, r)
			}
			if !ok {
				return
			}
			if !r.action || r.item != itemHomeDir || r.status != want {
				t.Errorf("row = %+v, want action, %q, %q", r, itemHomeDir, want)
			}
		})
	}
}

// TestAssess_HomeDirRow: the row reaches the assessment with its link, and
// is absent when the environment names no home.
func TestAssess_HomeDirRow(t *testing.T) {
	data := t.TempDir()
	cfg := config.DefaultConfig()
	find := func(a Assessment) *AssessmentRow {
		for i := range a.Assessment {
			if a.Assessment[i].Item == itemHomeDir {
				return &a.Assessment[i]
			}
		}
		return nil
	}

	r := find(Assess(context.Background(), cfg, Environment{DataDir: data, HomeDir: data}))
	if r == nil {
		t.Fatal("no Home directory row")
	}
	if !r.Action || r.Link != homeDirLink {
		t.Errorf("row = %+v", r)
	}
	if r := find(Assess(context.Background(), cfg, Environment{DataDir: data})); r != nil {
		t.Errorf("row without a home: %+v", r)
	}
}
