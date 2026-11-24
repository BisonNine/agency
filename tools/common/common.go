// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package common

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/fileutil"
	"go.bison9.com/agency/tools/files"
	"go.bison9.com/agency/utils"
)

// errResult builds an error Result carrying err both as the LLM-facing text and
// the internal Err field.
func errResult(format string, args ...any) *toolspec.Result {
	msg := fmt.Sprintf(format, args...)
	return &toolspec.Result{ForLLM: msg, IsError: true, Err: fmt.Errorf("%s", msg)}
}

// confine cleans rel and joins it under base, verifying the result stays within
// base (rejecting "../" escapes and absolute paths). It returns the absolute
// resolved path.
func confine(base, rel string) (string, error) {
	if base == "" {
		return "", errors.New("directory is not configured")
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("path is required")
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", rel)
	}
	abs := filepath.Join(base, cleaned)
	// Verify abs is base or a descendant of base.
	relCheck, err := filepath.Rel(base, abs)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the allowed directory: %q", rel)
	}
	return abs, nil
}

// mustRel is abs relative to base; confine has already put abs inside base.
func mustRel(base, abs string) string {
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return abs
	}
	return rel
}

// strArg extracts a string argument; ok is false when absent or empty.
func strArg(args map[string]any, key string) (string, bool) {
	v, present := args[key]
	if !present {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// location is a file named by a path inside a confining folder. The path is
// resolved inside that folder with os.Root, as the file tools' sandbox does:
// every component, links included, is opened relative to the folder, so a
// link cannot lead a read or a write outside it, and nothing can be swapped
// between a check and the use.
type location struct {
	base  string // the confining folder
	rel   string // the path inside it
	shown string // the path as the agent gave it, for a refusal
	area  string // what base is, for a refusal
	// scope, when set, is the agent's file-read permission: the source is
	// read only if the file tools could read it.
	scope *files.Reader
}

// refuse words err from the folder: a path that resolves outside it is
// refused as the file tools refuse it; anything else is returned as is.
func (l location) refuse(err error) error {
	if err != nil && files.IsRootEscape(err) {
		return toolkit.Refusal(fmt.Errorf("access denied: %s resolves outside %s", l.shown, l.area))
	}
	return err
}

// copyFile copies src to dst, each confined to its folder, creating
// intermediate folders for dst with exactly modes.Dir and dst with exactly
// modes.File (the destination area's modes), whatever the umask.
func copyFile(src, dst location, modes config.Modes) error {
	in, err := openSource(src)
	if err != nil {
		return err
	}
	defer utils.CloseQuietly(in)

	dstRoot, err := os.OpenRoot(dst.base)
	if err != nil {
		return err
	}
	defer utils.CloseQuietly(dstRoot)
	if mkErr := mkdirAllExact(dstRoot, filepath.Dir(dst.rel), modes.Dir); mkErr != nil {
		return dst.refuse(mkErr)
	}
	out, err := dstRoot.OpenFile(dst.rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, modes.File)
	if err != nil {
		return dst.refuse(err)
	}
	if err := out.Chmod(modes.File); err != nil {
		utils.CloseQuietly(out)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		utils.CloseQuietly(out)
		return err
	}
	return out.Close()
}

// openSource opens src inside its folder. A source with a scope is first
// opened through it, so a path the file tools would refuse is refused with
// their words, and must be the file opened in its folder. The caller closes
// the file.
func openSource(src location) (*os.File, error) {
	var scoped *os.File
	if src.scope != nil {
		f, err := src.scope.Open(filepath.Join(src.base, src.rel))
		if err != nil {
			return nil, err
		}
		defer utils.CloseQuietly(f)
		scoped = f
	}
	srcRoot, err := os.OpenRoot(src.base)
	if err != nil {
		return nil, err
	}
	defer utils.CloseQuietly(srcRoot)
	in, err := srcRoot.Open(src.rel)
	if err != nil {
		return nil, src.refuse(err)
	}
	if scoped != nil {
		if sameErr := sameFile(scoped, in); sameErr != nil {
			utils.CloseQuietly(in)
			return nil, fmt.Errorf("%s: %w", src.shown, sameErr)
		}
	}
	return in, nil
}

// sameFile returns an error unless a and b are the same file.
func sameFile(a, b *os.File) error {
	ai, err := a.Stat()
	if err != nil {
		return err
	}
	bi, err := b.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(ai, bi) {
		return errors.New("the file changed while it was being copied")
	}
	return nil
}

// mkdirAllExact creates rel under root and any missing parents, each with
// exactly mode, never leaving root.
func mkdirAllExact(root *os.Root, rel string, mode os.FileMode) error {
	rel = filepath.Clean(rel)
	if rel == "." {
		return nil
	}
	if fi, err := root.Stat(rel); err == nil {
		if fi.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: rel, Err: os.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := mkdirAllExact(root, filepath.Dir(rel), mode); err != nil {
		return err
	}
	if err := root.Mkdir(rel, mode); err != nil {
		if os.IsExist(err) {
			return nil // created concurrently
		}
		return err
	}
	return root.Chmod(rel, mode)
}

// copyResult is the result of a failed copy: a refusal as itself, any other
// failure as a failed copy.
func copyResult(tool string, err error) *toolspec.Result {
	if toolkit.IsExpectedRefusal(err) {
		msg := fmt.Sprintf("%s: %v", tool, err)
		return &toolspec.Result{ForLLM: msg, IsError: true, Err: err}
	}
	return errResult("%s: copy failed: %v", tool, err)
}

// listCommon lists files (name + size) under commonDir[/subdir].
func listCommon(commonDir string, args map[string]any) *toolspec.Result {
	dir := commonDir
	if subdir, ok := strArg(args, "subdir"); ok {
		abs, err := confine(commonDir, subdir)
		if err != nil {
			return errResult("common_list: %v", err)
		}
		dir = abs
	}
	if commonDir == "" {
		return errResult("common_list: common directory is not configured")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &toolspec.Result{ForLLM: "(empty)"}
		}
		return errResult("common_list: %v", err)
	}

	var b strings.Builder
	count := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
		} else {
			fmt.Fprintf(&b, "%s\t%d bytes\n", e.Name(), info.Size())
		}
		count++
	}
	if count == 0 {
		return &toolspec.Result{ForLLM: "(empty)"}
	}
	return &toolspec.Result{ForLLM: strings.TrimRight(b.String(), "\n")}
}

// getCommon copies commonDir/<name> into the agent's write folder as
// <as|basename(name)>. It writes only what writer, the agent's file-write
// permission, allows, and the file takes exactly fileMode.
func getCommon(commonDir, workspace string, fileMode os.FileMode, writer *files.Writer, args map[string]any) *toolspec.Result {
	name, ok := strArg(args, "name")
	if !ok {
		return errResult("common_get: 'name' is required")
	}
	if workspace == "" {
		return errResult("common_get: workspace is not configured")
	}
	srcAbs, err := confine(commonDir, name)
	if err != nil {
		return errResult("common_get: %v", err)
	}

	dstRel := filepath.Base(name)
	if as, ok := strArg(args, "as"); ok {
		dstRel = as
	}
	// as is a path inside the agent's write folder; the file tools decide
	// whether the agent may write there.
	writeFolder := filepath.Join(workspace, writer.Folder())
	dstAbs, err := confine(writeFolder, dstRel)
	if err != nil {
		return errResult("common_get: %v", err)
	}
	dst := mustRel(workspace, dstAbs)

	src := location{base: commonDir, rel: mustRel(commonDir, srcAbs), shown: name, area: "the common folder"}
	in, err := openSource(src)
	if err != nil {
		return copyResult("common_get", err)
	}
	data, err := io.ReadAll(in)
	utils.CloseQuietly(in)
	if err != nil {
		return copyResult("common_get", err)
	}
	if err := writer.WriteFile(dst, data, fileMode); err != nil {
		return copyResult("common_get", err)
	}
	return &toolspec.Result{ForLLM: fmt.Sprintf("Copied %q to workspace %s", name, dst)}
}

// putCommon copies <workspace>/<path> into commonDir/<as|basename(path)>,
// creating what it writes with the common area's modes. It reads only what
// reader, the agent's file-read permission, allows.
func putCommon(commonDir, workspace string, commonModes config.Modes, reader *files.Reader, args map[string]any) *toolspec.Result {
	path, ok := strArg(args, "path")
	if !ok {
		return errResult("common_put: 'path' is required")
	}
	if workspace == "" {
		return errResult("common_put: workspace is not configured")
	}
	if commonDir == "" {
		return errResult("common_put: common directory is not configured")
	}
	// The source is read only if the file tools could read it.
	srcAbs, err := confine(workspace, path)
	if err != nil {
		return errResult("common_put: %v", err)
	}

	dstRel := filepath.Base(path)
	if as, ok := strArg(args, "as"); ok {
		dstRel = as
	}
	dstAbs, err := confine(commonDir, dstRel)
	if err != nil {
		return errResult("common_put: %v", err)
	}

	// Create the common directory on first write.
	if err := fileutil.MkdirAllExact(commonDir, commonModes.Dir); err != nil {
		return errResult("common_put: %v", err)
	}
	src := location{base: workspace, rel: mustRel(workspace, srcAbs), shown: path, area: "the workspace", scope: reader}
	dst := location{base: commonDir, rel: mustRel(commonDir, dstAbs), shown: dstRel, area: "the common folder"}
	if err := copyFile(src, dst, commonModes); err != nil {
		return copyResult("common_put", err)
	}
	return &toolspec.Result{ForLLM: fmt.Sprintf("Copied workspace %q to common %s", path, dstRel)}
}

// deleteCommon removes commonDir/<name>.
func deleteCommon(commonDir string, args map[string]any) *toolspec.Result {
	name, ok := strArg(args, "name")
	if !ok {
		return errResult("common_delete: 'name' is required")
	}
	abs, err := confine(commonDir, name)
	if err != nil {
		return errResult("common_delete: %v", err)
	}
	if err := os.Remove(abs); err != nil {
		return errResult("common_delete: %v", err)
	}
	return &toolspec.Result{ForLLM: fmt.Sprintf("Deleted %q from common", name)}
}
