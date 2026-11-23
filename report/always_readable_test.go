// ClawEh
// License: MIT

package report

import (
	"testing"

	"go.bison9.com/agency/config"
)

// Every always-readable workspace folder has a note in the Folder access table.
func TestAlwaysReadableNotesEdgeerDirs(t *testing.T) {
	for _, dir := range config.AlwaysReadableWorkspaceDirs {
		if alwaysReadableNotes[dir] == "" {
			t.Errorf("no note for always-readable folder %q", dir)
		}
	}
}
