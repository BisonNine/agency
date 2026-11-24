// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package maestro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/kbhost"
	"go.bison9.com/agency/tools"
	toolsknowledge "go.bison9.com/agency/tools/knowledge"
)

// knowledgeEnv is a store installed for the process, and alice (knowledge
// bases on, with a shared one) and bob (off).
type knowledgeEnv struct {
	store  *kb.Store
	cfg    *config.Config
	ws     string
	shared string
}

func newKnowledgeEnv(t *testing.T) *knowledgeEnv {
	t.Helper()
	store, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	toolsknowledge.SetStore(store)
	t.Cleanup(func() {
		toolsknowledge.SetStore(nil)
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	e := &knowledgeEnv{store: store, ws: filepath.Join(dir, "alice"), shared: filepath.Join(dir, "standards")}
	if err := os.Mkdir(e.shared, 0o700); err != nil {
		t.Fatal(err)
	}
	e.cfg = &config.Config{Agents: config.AgentsConfig{List: []config.AgentConfig{
		{
			ID: "alice", Workspace: e.ws, Maestro: &config.MaestroConfig{Enabled: true},
			Knowledge: &config.KnowledgeConfig{Enabled: true, Shared: []config.SharedKnowledgeConfig{{Name: "cc", Path: e.shared, Write: true}}},
		},
		{ID: "bob", Maestro: &config.MaestroConfig{Enabled: true}},
	}}}
	return e
}

// put writes a concept with the store, as kb_put would.
func (e *knowledgeEnv) put(t *testing.T, kbName, id string, fm kb.Frontmatter, body string) {
	t.Helper()
	r := kbhost.New(e.cfg, e.cfg.AgentByID("alice"), e.ws)
	if _, err := e.store.Put(context.Background(), r, kbName, id, fm, body); err != nil {
		t.Fatalf("put %s/%s: %v", kbName, id, err)
	}
}

// The adapter reads records, with their includes and sources, and searches,
// over the agent's own and shared knowledge bases; an unknown one is an
// error.
func TestKnowledgeSource(t *testing.T) {
	e := newKnowledgeEnv(t)
	ctx := context.Background()
	e.put(t, "notes", "sfr/fcs-ckm", kb.Frontmatter{"type": "Requirement", "title": "Key generation"}, "Keys are 256 bits.")
	e.put(t, "notes", "sfr/missing", kb.Frontmatter{"type": "Requirement", "title": "Gone"}, "removed below")
	e.put(t, "notes", "sfr/fcs-cop", kb.Frontmatter{
		"type": "Requirement", "title": "Cryptographic operation",
		"sources":     []any{map[string]any{"resource": "CC Part 2"}, map[string]any{"resource": "PP 3.1", "note": "section 4"}},
		kb.IncludeKey: []any{"sfr/fcs-ckm", "sfr/missing"},
	}, "AES only.")
	// An include removed by hand after it was written is missing.
	if err := os.Remove(filepath.Join(kbhost.OwnDir(e.ws), "notes", "sfr", "missing.md")); err != nil {
		t.Fatal(err)
	}
	e.put(t, "shared/cc", "guide/crypto", kb.Frontmatter{"type": "Guide", "title": "Crypto review"}, "# Key sizes\n\nCheck the key sizes.")

	src := knowledgeFor(e.cfg, "alice", e.ws)
	if src == nil {
		t.Fatal("no knowledge source for an agent with knowledge bases")
	}
	rec, err := src.Get(ctx, "notes", "sfr/fcs-cop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ID != "sfr/fcs-cop" || rec.Title != "Cryptographic operation" || !strings.Contains(rec.Body, "AES only.") {
		t.Errorf("record = %+v", rec)
	}
	if !reflect.DeepEqual(rec.Sources, []string{"CC Part 2", "PP 3.1"}) {
		t.Errorf("sources = %v", rec.Sources)
	}
	if len(rec.Includes) != 1 || rec.Includes[0].ID != "sfr/fcs-ckm" || rec.Includes[0].Title != "Key generation" {
		t.Errorf("includes = %+v", rec.Includes)
	}
	if !reflect.DeepEqual(rec.Missing, []string{"sfr/missing"}) {
		t.Errorf("missing = %v", rec.Missing)
	}

	hits, err := src.Search(ctx, "shared/cc", "key sizes", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("Search = %+v, %v", hits, err)
	}
	if hits[0].ID != "guide/crypto" || hits[0].Title != "Crypto review" || hits[0].Section != "Key sizes" || hits[0].Passage == "" {
		t.Errorf("hit = %+v", hits[0])
	}
	if hits, err := src.Search(ctx, "notes", "nothing like this", 5); err != nil || len(hits) != 0 {
		t.Errorf("no match = %+v, %v", hits, err)
	}

	if _, err := src.Get(ctx, "shared/other", "x"); !errors.Is(err, kb.ErrUnknownKB) {
		t.Errorf("unknown kb Get = %v", err)
	}
	if _, err := src.Search(ctx, "shared/other", "x", 5); !errors.Is(err, kb.ErrUnknownKB) {
		t.Errorf("unknown kb Search = %v", err)
	}
	if _, err := src.Get(ctx, "notes", "sfr/none"); err == nil {
		t.Error("a missing record was found")
	}
}

// Only an agent with knowledge bases, in a process with a store, gets a
// source, and only then do Maestro's tasks take the knowledge parameter. A
// clone, registered with its source's id and workspace, reads its source's
// knowledge bases.
func TestKnowledgeParameter(t *testing.T) {
	e := newKnowledgeEnv(t)
	if knowledgeFor(e.cfg, "bob", t.TempDir()) != nil {
		t.Error("bob (knowledge off) has a knowledge source")
	}
	hasKnowledge := func(agentID, ws string, temp bool) bool {
		t.Helper()
		defs := GlobalProvider.RegisterTools(toolspec.Deps{
			Cfg: e.cfg, AgentID: agentID,
			Host:  tools.ToolDeps{Workspace: ws, TempAgent: temp},
			Spawn: &fakeRunner{out: "ok"},
		})
		for _, d := range defs {
			if d.Name == "task_create" {
				for _, p := range d.Parameters {
					if p.Name == "knowledge" {
						return true
					}
				}
				return false
			}
		}
		t.Fatalf("%s: no task_create", agentID)
		return false
	}
	if !hasKnowledge("alice", e.ws, false) {
		t.Error("alice's tasks take no knowledge")
	}
	e.put(t, "notes", "a", kb.Frontmatter{"type": "Note", "title": "A"}, "body")
	// The clone shares its source's runner and resolves its source's bases.
	if !hasKnowledge("alice", e.ws, true) {
		t.Error("alice's clone's tasks take no knowledge")
	}
	if rec, err := knowledgeFor(e.cfg, "alice", e.ws).Get(context.Background(), "notes", "a"); err != nil || rec.Title != "A" {
		t.Errorf("clone's source Get = %+v, %v", rec, err)
	}
	if hasKnowledge("bob", filepath.Join(t.TempDir(), "bob"), false) {
		t.Error("bob's tasks take knowledge")
	}
	toolsknowledge.SetStore(nil)
	if knowledgeFor(e.cfg, "alice", e.ws) != nil || hasKnowledge("alice", e.ws, false) {
		t.Error("without a store alice still has a knowledge source")
	}
}
