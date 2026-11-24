// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package maestro

import (
	"context"
	"errors"
	"slices"

	"go.bison9.com/agent/toolbox/kb"
	mknowledge "go.bison9.com/maestro/knowledge"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/kbhost"
	toolsknowledge "go.bison9.com/agency/tools/knowledge"
)

// knowledgeSource is Maestro's knowledge.Source over the process's
// knowledge-base store and one agent's resolver: the records a task names
// are read, and its queries searched, by the same functions the agent's
// kb_get and kb_search use, with the knowledge bases the agent can reach.
type knowledgeSource struct {
	store    *kb.Store
	resolver kb.Resolver
	// noSearch: the agent's kb_search is denied, so a task's query is too.
	noSearch bool
}

// errSearchDenied refuses a task's query for an agent whose kb_search is
// denied.
var errSearchDenied = errors.New("searching knowledge bases is not allowed for this agent")

var _ mknowledge.Source = knowledgeSource{}

// knowledgeFor is the knowledge source of agentID's Maestro tasks, reading
// what the agent's own tools may: nil when the agent has no knowledge bases
// (its `knowledge` switch is off), its kb_get is denied, or the process has
// no store, so the tasks' knowledge parameter is not offered; with kb_search
// denied, a task's query is refused. A clone passes its source's id and
// workspace, so it has its source's knowledge bases.
func knowledgeFor(c *config.Config, agentID, workspace string) mknowledge.Source {
	if !c.AgentSuiteEnabled(agentID, toolsknowledge.Suite) {
		return nil
	}
	agent := c.AgentByID(agentID)
	if agent.IsToolDenied(toolsknowledge.Namespace + "_get") {
		return nil
	}
	store := toolsknowledge.Store()
	if store == nil {
		return nil
	}
	return knowledgeSource{
		store: store, resolver: kbhost.New(c, agent, workspace),
		noSearch: agent.IsToolDenied(toolsknowledge.Namespace + "_search"),
	}
}

// Get implements knowledge.Source.
func (s knowledgeSource) Get(ctx context.Context, kbName, id string) (*mknowledge.Record, error) {
	rec, err := s.store.Get(ctx, s.resolver, kbName, id)
	if err != nil {
		return nil, err
	}
	out := recordOf(rec)
	return &out, nil
}

// Search implements knowledge.Source.
func (s knowledgeSource) Search(ctx context.Context, kbName, query string, limit int) ([]mknowledge.Hit, error) {
	if s.noSearch {
		return nil, errSearchDenied
	}
	hits, err := s.store.Search(ctx, s.resolver, kbName, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mknowledge.Hit, 0, len(hits))
	for _, h := range hits {
		out = append(out, mknowledge.Hit{ID: h.ID, Title: h.Title, Section: h.Section, Passage: h.Passage})
	}
	return out, nil
}

// recordOf maps a kb record, with its includes, to Maestro's.
func recordOf(rec *kb.Record) mknowledge.Record {
	out := mknowledge.Record{
		ID:      rec.ID,
		Title:   rec.Frontmatter.Title(),
		Body:    rec.Body,
		Sources: sourcesOf(rec.Frontmatter),
		Missing: slices.Clone(rec.Missing),
		Omitted: slices.Clone(rec.Omitted),
	}
	for i := range rec.Includes {
		out.Includes = append(out.Includes, recordOf(&rec.Includes[i]))
	}
	return out
}

// sourcesOf lists the resource of each of the record's sources entries (kb
// refuses a record with an entry that names none).
func sourcesOf(fm kb.Frontmatter) []string {
	var out []string
	for _, src := range fm.Sources() {
		if r, ok := src["resource"].(string); ok {
			out = append(out, r)
		}
	}
	return out
}
