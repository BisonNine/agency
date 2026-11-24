// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// This file adapts a toolspec provider (toolspec.ToolProvider) into a host
// ToolProvider. A tool package implements toolspec.ToolProvider with BARE tool
// names; NamespacedProvider mounts it under a namespace, so the published tool
// name is "<namespace>_<bare>". Each definition becomes a Tool through the
// SDK's bridge (toolkit.FromSpec), which satisfies AsyncExecutor and
// SessionScoped exactly when the definition says so.

package tools

import (
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

// ---- Provider adapter ------------------------------------------------------

// namespacedProvider adapts a toolspec.ToolProvider into a tools.ToolProvider,
// applying the namespace prefix to every tool it produces.
type namespacedProvider struct {
	ns string
	p  toolspec.ToolProvider
	// bare suppresses the "<ns>_" tool-name prefix while keeping ns as the
	// suite/catalog identity — for providers whose tool names are already
	// globally unique (e.g. fusion's service-prefixed names). See namePrefix.
	bare bool
}

// NamespacedProvider mounts a toolspec.ToolProvider under ns. The resulting
// provider plugs into RegisterProvider/GetProviders exactly like a legacy one.
func NamespacedProvider(ns string, p toolspec.ToolProvider) ToolProvider {
	return namespacedProvider{ns: ns, p: p}
}

// BareNamespacedProvider is like NamespacedProvider but publishes each tool under
// its own bare name (no "<ns>_" prefix). ns still groups the tools as a suite and
// names the catalog/config entry; the caller guarantees the tool names are
// globally unique.
func BareNamespacedProvider(ns string, p toolspec.ToolProvider) ToolProvider {
	return namespacedProvider{ns: ns, p: p, bare: true}
}

// namePrefix is the "<ns>" passed to toolkit.FromSpec ("" in bare mode).
func (a namespacedProvider) namePrefix() string {
	if a.bare {
		return ""
	}
	return a.ns
}

func (a namespacedProvider) Namespace() string { return a.ns }

func (a namespacedProvider) Description() string {
	if m, ok := a.p.(toolspec.HostMeta); ok {
		return m.Description()
	}
	return a.ns
}

func (a namespacedProvider) Category() string  { return a.ns }
func (a namespacedProvider) ConfigKey() string { return a.ns }

// Suite reports the all-or-nothing suite name when the wrapped provider declares
// itself one (cogmem, maestro); "" otherwise.
func (a namespacedProvider) Suite() string {
	if sp, ok := a.p.(SuiteProvider); ok {
		return sp.Suite()
	}
	return ""
}

func (a namespacedProvider) Available(cfg *config.Config) (bool, string) {
	if m, ok := a.p.(toolspec.HostMeta); ok {
		return m.Available(cfg)
	}
	return true, ""
}

func (a namespacedProvider) Build(deps ToolDeps) []toolkit.Tool {
	// All-or-nothing suite: gated as a unit by the per-agent flag. When enabled,
	// every tool is registered (bypassing the per-tool allow/deny gate — the flag
	// IS the allow decision); when disabled, none are. The provider's RegisterTools
	// already returns nothing for a disabled agent, but gate here too.
	if suite := a.Suite(); suite != "" && deps.Cfg != nil && !deps.Cfg.AgentSuiteEnabled(deps.AgentID, suite) {
		return nil
	}
	defs := a.p.RegisterTools(a.toGlobalDeps(deps))
	out := make([]toolkit.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, toolkit.FromSpec(a.namePrefix(), d))
	}
	return out
}

func (a namespacedProvider) Describe() []ToolDescriptor {
	// All-or-nothing suites collapse to a single GUI entry (managed by the agent
	// toggle), so the per-tool catalog is not cluttered with the whole suite.
	if suite := a.Suite(); suite != "" {
		desc := a.ns
		if m, ok := a.p.(toolspec.HostMeta); ok {
			desc = m.Description()
		}
		return []ToolDescriptor{{
			Name:        a.ns,
			Description: desc,
			Suite:       suite,
		}}
	}

	// Enumeration is deps-free: global providers must return their full tool set
	// from RegisterTools and build handler closures lazily (no eager deref of
	// deps), so a zero Deps is safe for cataloguing.
	defs := a.p.RegisterTools(toolspec.Deps{})
	out := make([]ToolDescriptor, 0, len(defs))
	for _, d := range defs {
		out = append(out, ToolDescriptor{
			Name:           a.ns + "_" + d.Name,
			Description:    d.Description,
			DefaultEnabled: d.DefaultAllowed(), // default-deny: only explicit DefaultAllow(true)
		})
	}
	return out
}

// toGlobalDeps converts Claw's rich ToolDeps into the portable toolspec.Deps,
// passing the full ToolDeps through Host so converted packages can recover their
// strongly-typed dependencies via deps.Host.(tools.ToolDeps).
func (a namespacedProvider) toGlobalDeps(deps ToolDeps) toolspec.Deps {
	return toolspec.Deps{
		Cfg:       deps.Cfg,
		AgentID:   deps.AgentID,
		Workspace: deps.Workspace,
		Spawn:     deps.Spawn,
		Host:      deps,
	}
}
