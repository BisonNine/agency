// ClawEh
// License: MIT

package config

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// KnowledgeSuite is the per-agent switch (AgentSuiteEnabled) that gives an
// agent knowledge bases and the kb_* tools.
const KnowledgeSuite = "knowledge"

// KnowledgeConfig is an agent's `knowledge` block. Enabled (off by default)
// gives the agent knowledge bases of its own in its workspace and the
// knowledge-base tools; Shared lists folders the operator shares with it.
type KnowledgeConfig struct {
	Enabled bool                    `json:"enabled"`
	Shared  []SharedKnowledgeConfig `json:"shared,omitempty"`
}

// SharedKnowledgeConfig is one knowledge base an operator shares with an
// agent: Name is how the agent addresses it (after the "shared/" prefix),
// Path the absolute folder holding the bundle, Write whether the agent may
// write it (default false).
type SharedKnowledgeConfig struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}

// KnowledgeEnabled reports whether the agent's knowledge switch is on.
func (a *AgentConfig) KnowledgeEnabled() bool {
	return a != nil && a.Knowledge != nil && a.Knowledge.Enabled
}

// SharedKnowledge returns the agent's shared knowledge bases, or nil.
func (a *AgentConfig) SharedKnowledge() []SharedKnowledgeConfig {
	if a == nil || a.Knowledge == nil {
		return nil
	}
	return a.Knowledge.Shared
}

// ValidSharedKnowledgeName reports whether name can name a shared knowledge
// base: letters, digits, '-', '_' and '.', not starting with a dot.
func ValidSharedKnowledgeName(name string) bool {
	if name == "" || name[0] == '.' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// knowledgeProblems returns one error for each shared knowledge base entry
// that cannot be used: a missing or bad name, a name used twice by the same
// agent, or a folder that is not an absolute path. LoadConfig and
// Store.Update refuse them (loadProblems).
func (c *Config) knowledgeProblems() []error {
	var errs []error
	for i := range c.Agents.List {
		a := &c.Agents.List[i]
		seen := map[string]bool{}
		for _, s := range a.SharedKnowledge() {
			who := a.DisplayName() + "'s shared knowledge base"
			switch {
			case s.Name == "":
				errs = append(errs, errors.New(who+" for "+strconv.Quote(s.Path)+" needs a name."))
			case !ValidSharedKnowledgeName(s.Name):
				errs = append(errs, errors.New(who+" name "+strconv.Quote(s.Name)+
					" may contain only letters, digits, -, _ and . and may not start with a dot."))
			case seen[s.Name]:
				errs = append(errs, errors.New(a.DisplayName()+" has more than one shared knowledge base named "+strconv.Quote(s.Name)+"."))
			}
			seen[s.Name] = true
			if !filepath.IsAbs(strings.TrimSpace(s.Path)) {
				errs = append(errs, errors.New(who+" "+strconv.Quote(s.Name)+" must have an absolute folder path."))
			}
		}
	}
	return errs
}

// MissingSharedKnowledge is a shared knowledge base whose folder does not
// exist or is not a directory: the agent does not have it.
type MissingSharedKnowledge struct {
	Agent string // agent id
	Name  string
	Path  string // as configured
}

// MissingSharedKnowledgeFolders lists, in config order, every shared
// knowledge base with an absolute path whose folder is not an existing
// directory.
func (c *Config) MissingSharedKnowledgeFolders() []MissingSharedKnowledge {
	var out []MissingSharedKnowledge
	for i := range c.Agents.List {
		a := &c.Agents.List[i]
		for _, s := range a.SharedKnowledge() {
			p := strings.TrimSpace(s.Path)
			if filepath.IsAbs(p) && !isDir(p) {
				out = append(out, MissingSharedKnowledge{Agent: a.ID, Name: s.Name, Path: s.Path})
			}
		}
	}
	return out
}
