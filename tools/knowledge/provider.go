// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package knowledge mounts the SDK's knowledge-base tools
// (go.bison9.com/agent/toolbox/kb/tools) under the "kb" namespace (kb_get,
// kb_search, kb_put), gated by the agent's `knowledge` switch, with the
// agent's own kbhost.Resolver. A clone acts as its source (deps.AgentID and
// the source's workspace), so it has its source's knowledge bases.
package knowledge

import (
	"sync/atomic"

	"go.bison9.com/agent/toolbox/kb"
	kbtools "go.bison9.com/agent/toolbox/kb/tools"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/kbhost"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// Suite is the per-agent switch that gates the tools (`knowledge`).
const Suite = config.KnowledgeSuite

// Namespace is the tools' name prefix (kb_get, ...).
const Namespace = kbtools.Namespace

// store is the process's one knowledge-base store (SetStore).
var store atomic.Pointer[kb.Store]

// SetStore installs the store the tools run on. The gateway calls it once,
// before the agent loop builds any agent's tools; without it no agent gets
// knowledge-base tools.
func SetStore(s *kb.Store) { store.Store(s) }

// Store returns the installed store, nil when there is none (Maestro reads
// the records its tasks name through it).
func Store() *kb.Store { return store.Load() }

// GlobalProvider mounts the knowledge-base tools. It is an all-or-nothing
// suite gated by the per-agent `knowledge` switch (default off).
var GlobalProvider globalKnowledgeProvider

type globalKnowledgeProvider struct{}

func (globalKnowledgeProvider) Namespace() string { return Namespace }
func (globalKnowledgeProvider) Description() string {
	return "Knowledge bases: read, search and write OKF bundles"
}
func (globalKnowledgeProvider) Available(any) (bool, string) { return true, "" }

// Suite marks the tools as a suite gated by the agent's `knowledge` switch.
func (globalKnowledgeProvider) Suite() string { return Suite }

// RegisterTools returns kb_get, kb_search and kb_put (bare names) when the
// agent's switch is on and the process has a store.
func (globalKnowledgeProvider) RegisterTools(deps toolspec.Deps) []toolspec.ToolDefinition {
	c, ok := deps.Cfg.(*config.Config)
	if !ok || c == nil {
		// Enumeration pass: the suite is one toggle, not catalogue entries.
		return nil
	}
	if !c.AgentSuiteEnabled(deps.AgentID, Suite) {
		return nil
	}
	host, ok := deps.Host.(tools.ToolDeps)
	if !ok || host.Workspace == "" {
		logger.WarnCF("kb", "no workspace for agent; knowledge-base tools disabled",
			map[string]any{"agent": deps.AgentID})
		return nil
	}
	resolver := kbhost.New(c, c.AgentByID(deps.AgentID), host.Workspace)
	t, err := kbtools.New(kbtools.Config{
		Store:    store.Load(),
		Resolver: func(*toolspec.ToolCall) kb.Resolver { return resolver },
	})
	if err != nil {
		// No store: only the gateway serves knowledge bases.
		logger.DebugCF("kb", "no knowledge-base store in this process; no knowledge-base tools",
			map[string]any{"agent": deps.AgentID, "error": err.Error()})
		return nil
	}
	return t.Definitions()
}
