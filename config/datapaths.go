// Agency
// License: MIT

package config

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// ToStoredPath returns path in the form Agency writes it into a file of its
// data directory (home): a path inside home becomes relative to it, with
// forward slashes, so the directory can be renamed or moved and still work.
// A path outside home, and a path that is already relative, is returned as
// it is.
func ToStoredPath(home, path string) string {
	if home == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

// ResolvePath returns the path a stored path names: a leading "~" is the
// user's home directory, a relative path is relative to home (the data
// directory), and an absolute path is used as it is. Every read of a path
// kept in a data-directory file goes through here.
func ResolvePath(home, path string) string {
	if path == "" {
		return path
	}
	path = ExpandHome(path)
	if filepath.IsAbs(path) || home == "" {
		return path
	}
	return filepath.Join(home, filepath.FromSlash(path))
}

// resolvePaths turns every path setting of c into the path it names (see
// ResolvePath), so the running configuration, and everything that shows it,
// holds full paths whatever form config.json keeps them in.
func (c *Config) resolvePaths() {
	c.eachPath(func(p string) string {
		if p = strings.TrimSpace(p); p == "" {
			return p
		}
		return ResolvePath(c.dataDir, p)
	})
}

// withStoredPaths returns a copy of c whose path settings are in their
// stored form (see ToStoredPath), for writing config.json. c is not changed.
func (c *Config) withStoredPaths() *Config {
	out := *c
	out.Agents.List = slices.Clone(c.Agents.List)
	out.Models = slices.Clone(c.Models)
	out.Tools.MCP.Servers = maps.Clone(c.Tools.MCP.Servers)
	out.eachPath(func(p string) string { return ToStoredPath(c.dataDir, p) })
	return &out
}

// eachPath replaces every path setting that may name a place in the data
// directory with fn of it. Allowed folders and shared knowledge bases are
// not among them: they are absolute paths an operator chose, outside the
// data directory.
func (c *Config) eachPath(fn func(string) string) {
	for _, p := range []*string{
		&c.Agents.BaseDir, &c.Agents.CommonDir,
		&c.Gateway.TLS.CertFile, &c.Gateway.TLS.KeyFile,
		&c.Backup.Dest,
	} {
		*p = fn(*p)
	}
	for i := range c.Agents.List {
		c.Agents.List[i].Workspace = fn(c.Agents.List[i].Workspace)
	}
	for i := range c.Models {
		c.Models[i].Workspace = fn(c.Models[i].Workspace)
		c.Models[i].ResponseLogFile = fn(c.Models[i].ResponseLogFile)
	}
	for name, s := range c.Tools.MCP.Servers {
		s.EnvFile = fn(s.EnvFile)
		c.Tools.MCP.Servers[name] = s
	}
}
