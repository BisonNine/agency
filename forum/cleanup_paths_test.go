// ClawEh
// License: MIT

package forum

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// A base directory whose name holds glob metacharacters still finds and
// removes a forum's cleanup markers.
func TestCleanupMarkers_BaseWithGlobCharacters(t *testing.T) {
	for _, name := range []string{"ws[1]", "ws*", "ws?", `ws\x`} {
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), name)
			id := uuid.NewString()
			other := uuid.NewString()
			cleanup := filepath.Join(base, dirCleanup)
			if err := os.MkdirAll(filepath.Join(cleanup, id), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, m := range []string{id + ".1." + cleanupAgents, id + ".3." + cleanupAgents, id + ".x." + cleanupAgents, other + ".2." + cleanupAgents} {
				if err := os.WriteFile(filepath.Join(cleanup, m), []byte("[]"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			s := newForumHandle(base, id)
			runs, err := s.MarkedRuns(cleanupAgents)
			if err != nil || !slices.Equal(runs, []int{1, 3}) {
				t.Fatalf("MarkedRuns = %v, %v; want [1 3]", runs, err)
			}

			if err := removeStaged(base, id); err != nil {
				t.Fatalf("removeStaged: %v", err)
			}
			for _, p := range []string{filepath.Join(cleanup, id), filepath.Join(cleanup, id+".1."+cleanupAgents), filepath.Join(cleanup, id+".x."+cleanupAgents)} {
				if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s survives removeStaged (%v)", p, err)
				}
			}
			if _, err := os.Stat(filepath.Join(cleanup, other+".2."+cleanupAgents)); err != nil {
				t.Errorf("another forum's marker was removed: %v", err)
			}
		})
	}
}

func TestMarkedRuns_NoCleanupDir(t *testing.T) {
	runs, err := newForumHandle(t.TempDir(), uuid.NewString()).MarkedRuns(cleanupAgents)
	if err != nil || len(runs) != 0 {
		t.Fatalf("MarkedRuns = %v, %v; want none", runs, err)
	}
}
