// Agency
// License: MIT

package report

import (
	"path/filepath"
	"strings"
)

// itemHomeDir is the row shown when the process's home directory is the
// data directory or inside it.
const itemHomeDir = "Home directory"

// homeDirLink is the manual section that says how to move the home.
const homeDirLink = "/manual/getting-started/install.html#home-directory-in-the-data-directory"

// homeDirRow is the assessment row for a home directory inside the data
// directory: the CLIs the process starts see that home, and the data
// directory's files lose their execute bits at every start. ok is false
// (no row) when either is empty or the home lies elsewhere.
func homeDirRow(homeDir, dataDir string) (row assessRow, ok bool) {
	if strings.TrimSpace(homeDir) == "" || strings.TrimSpace(dataDir) == "" {
		return assessRow{}, false
	}
	if !pathWithin(resolvedPath(homeDir), resolvedPath(dataDir)) {
		return assessRow{}, false
	}
	return assessRow{true, itemHomeDir,
		"Home directory is inside the data directory (" + dataDir + "); CLI providers may not run."}, true
}

// resolvedPath is p cleaned, with its links resolved when that is possible.
func resolvedPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// pathWithin reports whether p is dir or a descendant of it, by path
// component: /opt/agency2 is not within /opt/agency.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
