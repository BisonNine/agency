// ClawEh
// License: MIT

package gateway

import (
	"fmt"

	"go.bison9.com/agent/toolbox/kb"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools/knowledge"
)

// newKnowledgeStore opens the process's one knowledge-base store and
// installs it for the knowledge-base tools. Called before the agent loop
// exists, because the loop builds every agent's tools as it starts. A
// reload keeps it: only the agents' resolvers, built with their tools, are
// new. Every bundle gets its modes from its resolver (kbhost), so the
// store's own are never used.
func newKnowledgeStore() (*kb.Store, error) {
	store, err := kb.New(kb.Options{Logger: logger.SDK()})
	if err != nil {
		return nil, fmt.Errorf("knowledge bases: %w", err)
	}
	knowledge.SetStore(store)
	return store, nil
}

// closeKnowledge uninstalls and closes the store. Called once no turn can
// call a knowledge-base tool any more.
func closeKnowledge(store *kb.Store) {
	if store == nil {
		return
	}
	knowledge.SetStore(nil)
	if err := store.Close(); err != nil {
		logger.WarnCF("kb", "knowledge-base store not closed cleanly", map[string]any{"error": err.Error()})
	}
}
