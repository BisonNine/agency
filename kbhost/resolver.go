// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package kbhost is the host's side of the SDK's knowledge bases
// (go.bison9.com/agent/toolbox/kb): the one place that knows where an
// agent's knowledge bases live and how it names them. An agent with the
// `knowledge` switch has its own bundles, the directories directly under
// <workspace>/okf, addressed by their bare names, read-write and created by
// a put; and the shared bundles its configuration lists, addressed as
// "shared/<name>", read-only unless the entry allows writing, never created.
package kbhost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

const (
	// OwnDirName is the workspace folder that holds an agent's own bundles.
	OwnDirName = "okf"
	// SharedPrefix is how an agent addresses a shared bundle.
	SharedPrefix = "shared/"
)

// OwnDir is the folder of a workspace's own bundles.
func OwnDir(workspace string) string {
	return filepath.Join(workspace, OwnDirName)
}

// Resolver is kb.Resolver for one agent, built from its configuration.
type Resolver struct {
	folder string       // <workspace>/okf, absolute
	modes  fsmode.Modes // what a put creates in an own bundle
	shared []config.SharedKnowledgeConfig
}

var _ kb.Resolver = (*Resolver)(nil)

// New returns the resolver of agent (nil: no shared bundles) whose
// workspace is workspace. A clone passes its source's agent and workspace,
// so it has its source's knowledge bases. Own bundles take the modes cfg
// gives files under the workspace.
func New(cfg *config.Config, agent *config.AgentConfig, workspace string) *Resolver {
	folder := OwnDir(workspace)
	if abs, err := filepath.Abs(folder); err == nil {
		folder = abs
	}
	r := &Resolver{folder: folder}
	if cfg != nil {
		m := cfg.AgentModes(folder)
		r.modes = fsmode.Modes{Dir: m.Dir, File: m.File}
	}
	for _, s := range agent.SharedKnowledge() {
		p := strings.TrimSpace(s.Path)
		if !config.ValidSharedKnowledgeName(s.Name) || !filepath.IsAbs(p) {
			continue // refused on save and at load; never resolved
		}
		if slices.ContainsFunc(r.shared, func(x config.SharedKnowledgeConfig) bool { return x.Name == s.Name }) {
			continue
		}
		r.shared = append(r.shared, config.SharedKnowledgeConfig{Name: s.Name, Path: filepath.Clean(p), Write: s.Write})
	}
	return r
}

// Resolve implements kb.Resolver.
func (r *Resolver) Resolve(_ context.Context, name string) (kb.Bundle, error) {
	if rest, ok := strings.CutPrefix(name, SharedPrefix); ok {
		s, found := r.sharedNamed(rest)
		if !found {
			return kb.Bundle{}, kb.ErrUnknownKB
		}
		info, err := os.Stat(s.Path)
		if err != nil || !info.IsDir() {
			// A folder that is missing or not a directory: the agent does
			// not have this bundle.
			return kb.Bundle{}, kb.ErrUnknownKB
		}
		return kb.Bundle{Root: s.Path, Writable: s.Write, Modes: sharedModes(info.Mode())}, nil
	}
	if !ownName(name) {
		return kb.Bundle{}, kb.ErrUnknownKB
	}
	return kb.Bundle{
		Root:     filepath.Join(r.folder, name),
		Confine:  r.folder,
		Writable: true,
		Create:   true,
		Modes:    r.modes,
	}, nil
}

// Bundles implements kb.Resolver: the own bundles that exist, then the
// shared ones whose folder exists, each group sorted by name.
func (r *Resolver) Bundles(context.Context) ([]kb.Info, error) {
	var own []kb.Info
	entries, err := os.ReadDir(r.folder)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && ownName(e.Name()) {
			own = append(own, kb.Info{Name: e.Name(), Writable: true})
		}
	}
	var shared []kb.Info
	for _, s := range r.shared {
		if info, statErr := os.Stat(s.Path); statErr == nil && info.IsDir() {
			shared = append(shared, kb.Info{Name: SharedPrefix + s.Name, Writable: s.Write})
		}
	}
	byName := func(a, b kb.Info) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(own, byName)
	slices.SortFunc(shared, byName)
	return append(own, shared...), nil
}

// WatchPaths are the folders whose changes can change Bundles: the own
// bundles' folder and each shared folder.
func (r *Resolver) WatchPaths() []string {
	paths := make([]string, 0, 1+len(r.shared))
	paths = append(paths, r.folder)
	for _, s := range r.shared {
		paths = append(paths, s.Path)
	}
	return paths
}

func (r *Resolver) sharedNamed(name string) (config.SharedKnowledgeConfig, bool) {
	for _, s := range r.shared {
		if s.Name == name {
			return s, true
		}
	}
	return config.SharedKnowledgeConfig{}, false
}

// sharedModes are the modes a put creates in a shared bundle: those of its
// root folder, files without the execute bits. A shared folder's files are
// never made tighter than the folder the operator set up (a group-shared
// folder stays group-shared); a put that replaces a file keeps that file's
// own mode.
func sharedModes(root fs.FileMode) fsmode.Modes {
	perm := root.Perm()
	return fsmode.Modes{Dir: perm, File: perm &^ 0o111}
}

// ownName reports whether name can be an own bundle: one path segment, not
// empty, not hidden, with no separator or control character.
func ownName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, `/\`) &&
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// PromptText is the system-prompt text listing the bundles of r.
func PromptText(ctx context.Context, r kb.Resolver) string {
	infos, err := r.Bundles(ctx)
	if err != nil {
		logger.WarnCF("kb", "could not list the agent's knowledge bases", map[string]any{"error": err.Error()})
	}
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		n := i.Name
		if !i.Writable {
			n += " (read only)"
		}
		names = append(names, n)
	}
	list := "none yet"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	return "Bundles: " + list + "\n\n" +
		`Note: Bundle names may conflict. Do not assume that "mybundle" and "shared/mybundle" are the same.`
}
