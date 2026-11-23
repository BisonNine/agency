package files

import (
	"os"
	"path/filepath"
	"regexp"

	"go.bison9.com/agency/config"
)

// Writer writes files on an agent's behalf through exactly the fileSystem
// stack the file write tools use: the workspace sandbox, the allow-write
// patterns, the write folder (agents.defaults.workspace_write_subdir, with
// links resolved) and the allowed folders. A path a Writer may write is a
// path file_write would write, refused with the same words otherwise.
//
// Non-tool writers (common_get) use this so they cannot write where the
// agent's file tools could not.
type Writer struct {
	workspace string
	restrict  bool
	subdir    string
	allow     []*regexp.Regexp
}

// NewWriter builds a Writer for an agent's workspace from the config, as the
// file tools are built from it.
func NewWriter(c *config.Config, workspace string) *Writer {
	w := &Writer{workspace: workspace}
	if c != nil {
		w.restrict = c.Agents.Defaults.RestrictToWorkspace
		w.subdir = c.Agents.Defaults.WorkspaceWriteSubdir
		w.allow = compilePatterns(c.Tools.AllowWritePaths)
	}
	return w
}

// Folder is the agent's write folder relative to the workspace: the
// configured write subdir, or "" for the workspace itself.
func (w *Writer) Folder() string { return w.subdir }

// WriteFile writes data to path, a workspace-relative path, with exactly
// mode, creating missing folders as the file tools create them. The
// fileSystem is built per call, as Reader builds it, so it sees the allowed
// folders the file tools registered after the Writer was made.
func (w *Writer) WriteFile(path string, data []byte, mode os.FileMode) error {
	if !w.restrict && !filepath.IsAbs(path) {
		// Unrestricted writes go straight to the host: anchor the path at
		// the workspace, not the process's working directory.
		path = filepath.Join(w.workspace, path)
	}
	return buildWriteFs(w.workspace, w.restrict, w.subdir, w.allow).WriteFileMode(path, data, mode)
}
