// ClawEh
// License: MIT

package agent

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/models"

	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// taskUses are the task-model uses, each a purpose of every agent's set.
var taskUses = []config.TaskUse{
	config.TaskUseVision, config.TaskUseSTT, config.TaskUseTTS,
	config.TaskUseImage, config.TaskUseText, config.TaskUseClassify,
}

// taskPurpose is the purpose a task-model use is assigned under.
func taskPurpose(use config.TaskUse) models.Purpose { return models.Purpose(use) }

// agentModelLists are the lists agent inst's set holds, by purpose, in
// priority order (list position):
//   - Chat: its models (agents.list[].models, else the defaults');
//   - Worker: its worker models (subagents.models), when it has any; with
//     none the built-in Worker → Chat rule serves its sub-agents;
//   - Summarize and Memory: its summarization chain (agent list, then the
//     global one, then its primary model unless listed), the same for both;
//   - one host purpose per task-model use, from TaskModelsConfig.Entries,
//     each entry carried as the assignment's settings.
//
// A human agent has its chat model only: no model ever sees a person's
// conversation. A model c does not have (disabled, or refused when the
// catalogue was built) is left out. Every purpose is given, empty or not, so
// ReplaceAll leaves nothing stale.
func agentModelLists(c *models.Catalog, cfg *config.Config, inst *AgentInstance) map[models.Purpose][]models.Assignment {
	have := func(name string) bool { _, ok := c.Get(name); return ok }
	list := func(names []string, settings []any) []models.Assignment {
		out := make([]models.Assignment, 0, len(names))
		for i, n := range names {
			if !have(n) || slices.ContainsFunc(out, func(a models.Assignment) bool { return a.Name == n }) {
				continue
			}
			a := models.Assignment{Name: n}
			if settings != nil {
				a.Options = []models.AssignOption{models.WithSettings(settings[i])}
			}
			out = append(out, a)
		}
		return out
	}
	lists := map[models.Purpose][]models.Assignment{
		models.Chat:      list(inst.chatModels, nil),
		models.Worker:    nil,
		models.Summarize: nil,
		models.Memory:    nil,
	}
	if inst.HumanModel == "" {
		lists[models.Worker] = list(inst.workerModels, nil)
		summarize := list(summarizationNames(c, cfg, inst), nil)
		lists[models.Summarize], lists[models.Memory] = summarize, summarize
	}
	for _, use := range taskUses {
		var names []string
		var settings []any
		if inst.HumanModel == "" && cfg != nil {
			for _, e := range cfg.TaskModels.Entries(use) {
				names = append(names, e.Model)
				settings = append(settings, e)
			}
		}
		lists[taskPurpose(use)] = list(names, settings)
	}
	return lists
}

// assignAgentModels states agent inst's lists (agentModelLists) in c, all in
// one step: a turn sees the old lists or the new ones, never a mix, and a
// session's /model choice is kept while its model is still listed. Lists
// that did not change are left as they are.
func assignAgentModels(c *models.Catalog, cfg *config.Config, inst *AgentInstance) {
	if _, err := c.Set(inst.ID).ReplaceAll(agentModelLists(c, cfg, inst)); err != nil {
		// The lists hold only models c has, each once; a spent catalogue is
		// the only way left to fail.
		logger.WarnCF("agent", "agent's models not assigned",
			map[string]any{"agent_id": inst.ID, "error": err.Error()})
	}
}

// reloadAssignments ties the agents a reload builds to that reload. While
// it builds, an agent built is only recorded; once it has committed, the
// agents it installed get their lists (in the catalogue it adopts, or in the
// running one); if it is abandoned or fails, nothing it built reaches any
// catalogue. After a commit the reload's builder is the registry's, and an
// agent it builds then (created at run time) is assigned at once in the
// running catalogue.
type reloadAssignments struct {
	cfg  *config.Config
	next *models.Catalog

	mu    sync.Mutex
	state int // assignBuilding, assignCommitted or assignDropped
	built []*AgentInstance
}

const (
	assignBuilding = iota
	assignCommitted
	assignDropped
)

// build handles an agent the reload's builder made: it reports whether the
// agent is to be assigned in the running catalogue now (the reload has
// committed: a run-time creation).
func (r *reloadAssignments) build(inst *AgentInstance) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case assignBuilding:
		r.built = append(r.built, inst)
		return false
	case assignCommitted:
		return true
	default:
		return false // built by a reload that will never commit
	}
}

// end records how the reload ended and returns the agents it built.
func (r *reloadAssignments) end(committed bool) []*AgentInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = assignDropped
	if committed {
		r.state = assignCommitted
	}
	built := r.built
	r.built = nil
	return built
}

// assignBuilt states the models of an agent just built: as reload (the
// reload whose builder made it, nil for the registry's first builder)
// decides, else at once in the running catalogue.
func (al *AgentLoop) assignBuilt(reload *reloadAssignments, cfg *config.Config, inst *AgentInstance) {
	if reload != nil && !reload.build(inst) {
		return
	}
	assignAgentModels(al.catalog, cfg, inst)
}

// copyLists gives agent id the lists it has in from, in to: for an agent a
// reload did not rebuild, so adopting to keeps its lists and its sessions'
// /model choices. A model to does not have is left out.
func copyLists(from, to *models.Catalog, cfg *config.Config, id string) {
	src := from.Set(id)
	lists := make(map[models.Purpose][]models.Assignment)
	for _, p := range src.Purposes() {
		var list []models.Assignment
		for _, e := range src.List(p) {
			if raw, ok := providers.DerivedRaw(e.Name); ok {
				ensureDerived(to, cfg, raw)
			}
			if _, ok := to.Get(e.Name); !ok {
				continue
			}
			a := models.Assignment{Name: e.Name}
			if e.Settings != nil {
				a.Options = []models.AssignOption{models.WithSettings(e.Settings)}
			}
			list = append(list, a)
		}
		lists[p] = list
	}
	if _, err := to.Set(id).ReplaceAll(lists); err != nil {
		logger.WarnCF("agent", "agent's models not kept",
			map[string]any{"agent_id": id, "error": err.Error()})
	}
}

// summarizationNames is the agent's summarization chain as model names: its
// summarization_models, then the global summarization.models, de-duplicated,
// then its primary model unless already listed. A name that resolves to no
// enabled model is served as it always was, by the agents' default model
// with that name as its model id: a derived model (DerivedSummaryModel),
// added to c on first use. Without a default model it is left out.
func summarizationNames(c *models.Catalog, cfg *config.Config, inst *AgentInstance) []string {
	var agentModels, globalModels []string
	if inst.Config != nil {
		agentModels = inst.Config.SummarizationModels
	}
	if cfg != nil {
		globalModels = cfg.Summarization.Models
	}
	var out []string
	for _, raw := range append(resolveCompressModelChain(agentModels, globalModels), strings.TrimSpace(inst.Model)) {
		var name string
		switch alias, _, ok := resolveCompressModelTarget(cfg, raw); {
		case ok:
			name = alias
		case raw != "" && ensureDerived(c, cfg, raw):
			name = providers.DerivedSummaryName(raw)
		default:
			continue
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// catalogDigest is the digest of the catalogue cfg builds (providers.Digest):
// the providers, the model definitions, the cooldown policy and the client
// options, file modes included. A reload that changes it adopts a new
// catalogue, which clears every cooldown. The agents' lists (chat, worker,
// summarize, memory, task models) are not in it: a change to them alone
// replaces the lists in the running catalogue (ReplaceAll), and cooldowns
// stay.
func catalogDigest(cfg *config.Config) string {
	return providers.Digest(cfg)
}

// adoptCatalog hands next to the running catalogue and logs what changed.
// Cooldowns are not carried over: every model starts available.
func (al *AgentLoop) adoptCatalog(next *models.Catalog, digest string) {
	diff := al.catalog.Adopt(next)
	al.catalogDigest = digest
	// Every cooldown is cleared, as when the operator clears them: a model
	// alerted as failing alerts again at once if it still fails.
	al.catalogs.Alerts().RearmAll()
	fields := map[string]any{}
	if len(diff.Added) > 0 {
		fields["added"] = diff.Added
	}
	if len(diff.Removed) > 0 {
		fields["removed"] = diff.Removed
	}
	if len(diff.Changed) > 0 {
		fields["changed"] = diff.Changed
	}
	if len(diff.Reverted) > 0 {
		reverted := make([]string, 0, len(diff.Reverted))
		for _, r := range diff.Reverted {
			reverted = append(reverted, r.Set+"/"+string(r.Purpose)+": "+r.Model)
		}
		fields["selections_reverted"] = reverted
	}
	logger.InfoCF("agent", "Model settings changed; model cooldowns cleared", fields)
}

// listCooldowns lists the models that are parked now, once per provider and
// wire model (models that share both share their health), ordered by them.
func (al *AgentLoop) listCooldowns() []commands.CooldownEntry {
	byKey := make(map[string]commands.CooldownEntry)
	for _, h := range al.catalog.Health() {
		if h.Available {
			continue
		}
		provider, model, key := cooldownIdentity(h)
		byKey[key] = commands.CooldownEntry{
			Provider: provider, Model: model, Reason: string(h.Reason), Since: h.Since, Until: h.Until,
		}
	}
	keys := slices.Sorted(maps.Keys(byKey))
	out := make([]commands.CooldownEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

// clearCooldown clears the health of the models on provider with wire model
// model, and reports whether any had a record. Their alerts are re-armed: a
// failure after the clear alerts at once, a success reports recovery.
func (al *AgentLoop) clearCooldown(provider, model string) bool {
	var names []string
	for _, h := range al.catalog.Health() {
		if p, m, _ := cooldownIdentity(h); p == provider && m == model {
			names = append(names, h.Name)
		}
	}
	cleared := false
	if len(names) > 0 {
		cleared = al.catalog.ResetHealth(names...) > 0
	}
	al.catalogs.Alerts().Rearm(providers.ModelKey(provider, model))
	return cleared
}

// clearCooldowns clears the health of every model and re-arms every model's
// alerts.
func (al *AgentLoop) clearCooldowns() {
	al.catalog.ResetHealth()
	al.catalogs.Alerts().RearmAll()
}

// parkedFor returns how long the catalogue's model name stays parked: 0 when
// it can be called now, or when the catalogue does not have it.
func (al *AgentLoop) parkedFor(name string) time.Duration {
	info, ok := al.catalog.Get(name)
	if !ok || info.Health.Available {
		return 0
	}
	return max(al.clk().Until(info.Health.Until), 0)
}

// finishRecorder captures the last call a chain made: its response and its
// error as the client returned it, before the chain classified it.
type finishRecorder struct {
	called bool
	resp   *providers.LLMResponse
	err    error
}

// option is the chain option that records each call, after which emit (when
// set) is called with it.
func (r *finishRecorder) option(emit func(c models.Candidate, elapsed time.Duration, resp *providers.LLMResponse, err error)) models.CallOption {
	return models.WithFinish(func(c models.Candidate, elapsed time.Duration, resp *providers.LLMResponse, err error) {
		r.called, r.resp, r.err = true, resp, err
		if emit != nil {
			emit(c, elapsed, resp, err)
		}
	})
}

// setCandidates are the models of agent id's set for purpose p (its own
// list, not one a fallback rule leads to), in priority order, with their
// providers and wire models.
func (al *AgentLoop) setCandidates(id string, p models.Purpose) []providers.FallbackCandidate {
	return setCandidatesIn(al.catalog, id, p)
}

// setCandidatesIn is setCandidates in catalogue c.
func setCandidatesIn(c *models.Catalog, id string, p models.Purpose) []providers.FallbackCandidate {
	list := c.Set(id).List(p)
	out := make([]providers.FallbackCandidate, 0, len(list))
	for _, e := range list {
		if info, ok := c.Get(e.Name); ok {
			out = append(out, providers.FallbackCandidate{
				Name: e.Name, Provider: info.Provider.Name, Protocol: info.Provider.Protocol, Model: info.Model.Model,
			})
		}
	}
	return out
}

// chatCandidates are agent a's chat models (setCandidates).
func (al *AgentLoop) chatCandidates(a *AgentInstance) []providers.FallbackCandidate {
	return al.setCandidates(a.ID, models.Chat)
}

// workerCandidates are the models agent a's workers (sub-agents and Maestro
// dispatches) run on: its worker models when it has any configured (none
// when none of them is usable), else its chat models.
func (al *AgentLoop) workerCandidates(a *AgentInstance) []providers.FallbackCandidate {
	if a.Subagents != nil && len(a.Subagents.Models) > 0 {
		return al.setCandidates(a.ID, models.Worker)
	}
	return al.chatCandidates(a)
}

// ensureDerived adds to c, once, the derived summarization model for raw
// (providers.DerivedSummaryModel), and reports whether c has it.
func ensureDerived(c *models.Catalog, cfg *config.Config, raw string) bool {
	name := providers.DerivedSummaryName(raw)
	if _, ok := c.Get(name); ok {
		return true
	}
	m, ok := providers.DerivedSummaryModel(cfg, raw)
	if !ok {
		return false
	}
	if err := c.Add(m); err != nil {
		_, ok := c.Get(name) // added by a concurrent build
		return ok
	}
	return true
}

// cooldownIdentity is how /cooldowns names a model's health: its provider,
// wire model and key, or for a derived summarization model no provider and
// the raw name, as the summarization chain has always keyed it.
func cooldownIdentity(h models.Health) (provider, model, key string) {
	if raw, ok := providers.DerivedRaw(h.Name); ok {
		return "", raw, providers.ModelKey("", raw)
	}
	return h.Provider, h.Model, h.Key
}

// assignReloaded states the lists after a reload that committed: the agents
// it installed (built is what it built; one the registry did not take, as
// it turned busy, keeps its lists) get theirs from cfg. With next set (the
// reload changed what the catalogue is built from) they are assigned in
// next, every other agent the registry holds keeps the lists it has, and
// next is adopted; otherwise they are assigned in the running catalogue.
// Runs outside the registry's lock: the catalogue writes the sessions' model
// choices through the agents' stores, which it reaches through the registry.
func (al *AgentLoop) assignReloaded(registry *AgentRegistry, cfg *config.Config, next *models.Catalog, digest string, built []*AgentInstance) {
	installed := func(inst *AgentInstance) bool {
		cur, ok := registry.Get(inst.ID)
		return ok && cur == inst
	}
	if next == nil {
		for _, inst := range built {
			if installed(inst) {
				assignAgentModels(al.catalog, cfg, inst)
			}
		}
		return
	}
	for _, inst := range built {
		if installed(inst) {
			assignAgentModels(next, cfg, inst)
		}
	}
	for _, id := range registry.All() {
		if inst, ok := registry.Get(id); ok && inst != nil && !slices.Contains(built, inst) {
			copyLists(al.catalog, next, cfg, id)
		}
	}
	al.adoptCatalog(next, digest)
	// An agent created between the copy and the adoption has its lists only
	// in the catalogue the adoption replaced: give it its own.
	for _, id := range registry.All() {
		if inst, ok := registry.Get(id); ok && inst != nil && len(inst.chatModels) > 0 && len(al.catalog.Set(id).List(models.Chat)) == 0 {
			assignAgentModels(al.catalog, cfg, inst)
		}
	}
}
