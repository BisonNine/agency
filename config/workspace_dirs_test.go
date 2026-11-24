// Agency
// License: MIT

package config

import (
	"slices"
	"testing"
)

func TestShellJobsFolderAlwaysReadable(t *testing.T) {
	if !slices.Contains(AlwaysReadableWorkspaceDirs, ShellJobsDir) {
		t.Errorf("AlwaysReadableWorkspaceDirs = %v, lacks %s", AlwaysReadableWorkspaceDirs, ShellJobsDir)
	}
}
