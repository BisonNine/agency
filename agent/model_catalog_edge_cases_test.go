// Agency
// License: MIT

package agent

import (
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/models"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
)

// A spent catalogue takes no lists; the failure is logged.
func TestModelCatalog_AssignToSpentCatalogue(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	al, _ := hooksEdgeLoop(t)
	cfg := al.GetConfig()
	next := al.catalogs.Build(cfg)
	al.adoptCatalog(next, catalogDigest(cfg))
	assignAgentModels(next, cfg, al.GetRegistry().Default())
	if !strings.Contains(logs.String(), "agent's models not assigned") {
		t.Fatalf("the refused lists were not logged: %s", logs.String())
	}
}

// A retired instance gets no lists stated.
func TestModelCatalog_AssignOneRetired(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _ := hooksEdgeLoop(t)
	inst := al.GetRegistry().Default()
	before := setNames(al, inst.ID, models.Chat)
	inst.listsRetired = true
	if kept := al.noteSelectionAgents([]*AgentInstance{inst}); len(kept) != 0 {
		t.Fatalf("noteSelectionAgents kept a retired instance: %v", kept)
	}
	// Built with a configuration that lists another model: stating its lists
	// would change the set.
	other := *inst.builtWith
	other.Agents.Defaults.Models = []string{"no-such-model"}
	inst.builtWith = &other
	al.assignOne(inst)
	if got := setNames(al, inst.ID, models.Chat); !slices.Equal(got, before) {
		t.Fatalf("a retired instance had its lists changed: %v, want %v", got, before)
	}
}

// Adopting a catalogue that reverts a session's /model choice logs the
// reverted selection.
func TestModelCatalog_AdoptLogsRevertedSelections(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	base := t.TempDir()
	al := mustNewAgentLoop(t, abcConfig(base, "a", "b", "c"), bus.NewMessageBus(), &mockProvider{}, nil)
	selectModel(t, al, al.GetRegistry().Default(), "agent:main:main", "c")
	next := abcConfig(base, "a", "b")
	al.adoptCatalog(al.catalogs.Build(next), catalogDigest(next))
	out := logs.String()
	if !strings.Contains(out, "selections_reverted") || !strings.Contains(out, "main/chat: c") {
		t.Fatalf("the reverted selection was not logged: %s", out)
	}
}
