// ClawEh
// License: MIT

package agent

import (
	"maps"
	"slices"
	"strings"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

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
//     global one, then its primary chat model unless listed), the same for
//     both;
//   - one host purpose per task-model use, from TaskModelsConfig.Entries,
//     each entry carried as the assignment's settings.
//
// A human agent has its chat model only: no model ever sees a person's
// conversation. A model c does not have (disabled, or refused when the
// catalogue was built) is left out; nothing stands in for it. Every purpose
// is given, empty or not, so ReplaceAll leaves nothing stale.
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
	chat := list(inst.chatModels, nil)
	lists := map[models.Purpose][]models.Assignment{
		models.Chat:      chat,
		models.Worker:    nil,
		models.Summarize: nil,
		models.Memory:    nil,
	}
	if inst.HumanModel == "" {
		lists[models.Worker] = list(inst.workerModels, nil)
		names := summarizationNames(cfg, inst)
		if len(chat) > 0 {
			names = append(names, chat[0].Name) // the primary chat model
		}
		summarize := list(names, nil)
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
// that did not change are left as they are. cfg is the agent's own
// configuration.
func assignAgentModels(c *models.Catalog, cfg *config.Config, inst *AgentInstance) {
	if _, err := c.Set(inst.ID).ReplaceAll(agentModelLists(c, cfg, inst)); err != nil {
		// The lists hold only models c has, each once; a spent catalogue is
		// the only way left to fail.
		logger.WarnCF("agent", "agent's models not assigned",
			map[string]any{"agent_id": inst.ID, "error": err.Error()})
	}
}

// assignOne states agent inst's lists (its own configuration's) in the
// running catalogue. Every creation calls it once the agent is in the
// registry, before anything can start a turn on it; a list that is already
// what it should be is left as it is, so calling it again changes nothing.
func (al *AgentLoop) assignOne(inst *AgentInstance) {
	al.listsGate.RLock()
	defer al.listsGate.RUnlock()
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	// Deleted (and retired) since its insertion: nothing to state, and
	// nothing would ever remove what was stated for it.
	if cur, ok := al.GetRegistry().Get(inst.ID); !ok || cur != inst {
		return
	}
	// Before the lists: ReplaceAll saves kept choices through it.
	if len(al.noteSelectionAgents([]*AgentInstance{inst})) == 0 {
		return // retired meanwhile
	}
	assignAgentModels(al.catalog, inst.builtWith, inst)
}

// stateListsAfterCommit states every agent's lists after a reload
// committed, adopting a new catalogue when the digest changed, and then
// releases the listsGate the commit took, whatever happens.
func (al *AgentLoop) stateListsAfterCommit(registry *AgentRegistry, cfg *config.Config, digest string) {
	defer al.listsGate.Unlock()
	if digest != al.currentDigest() {
		al.adoptFor(registry, cfg, digest)
	} else {
		al.reconcileAll(registry)
	}
}

// reconcileAll is assignOne for every agent the registry holds.
func (al *AgentLoop) reconcileAll(registry *AgentRegistry) {
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	// Recorded before the lists: ReplaceAll saves kept choices through the
	// new instances, not the replaced (closed) ones. One retired meanwhile
	// is left out.
	instances := al.noteSelectionAgents(al.liveInstances(registry))
	assignAll(al.catalog, instances)
}

// liveInstances are the instances whose sets are stated: every one the
// registry holds, and for an id it no longer has, the latest instance under
// that id not retired yet (a configured agent dropped while in a turn, kept
// until the turn ends; removeSet). Each gets its own configuration's lists
// of the models the catalogue has. Called with assignMu held.
func (al *AgentLoop) liveInstances(registry *AgentRegistry) []*AgentInstance {
	instances := registryInstances(registry)
	al.unretiredMu.Lock()
	latest := make(map[string]*AgentInstance, len(al.unretired))
	for id, kept := range al.unretired {
		if len(kept) > 0 {
			latest[id] = kept[len(kept)-1]
		}
	}
	al.unretiredMu.Unlock()
	for id, inst := range latest {
		if _, held := registry.Get(id); !held {
			instances = append(instances, inst)
		}
	}
	return instances
}

// retired drops inst from the unretired instances and returns those still
// unretired under its id, oldest first.
func (al *AgentLoop) retired(inst *AgentInstance) []*AgentInstance {
	al.unretiredMu.Lock()
	defer al.unretiredMu.Unlock()
	inst.listsRetired = true
	kept := slices.DeleteFunc(al.unretired[inst.ID], func(i *AgentInstance) bool { return i == inst })
	if len(kept) == 0 {
		delete(al.unretired, inst.ID)
		return nil
	}
	al.unretired[inst.ID] = kept
	return slices.Clone(kept)
}

// noteSelectionAgents records instances as the ones their sets' choices are
// read and written through, and as not retired, and returns them less any
// already retired. Called with assignMu held.
func (al *AgentLoop) noteSelectionAgents(instances []*AgentInstance) []*AgentInstance {
	al.unretiredMu.Lock()
	defer al.unretiredMu.Unlock()
	if al.unretired == nil {
		al.unretired = make(map[string][]*AgentInstance)
	}
	kept := make([]*AgentInstance, 0, len(instances))
	for _, inst := range instances {
		if inst.listsRetired {
			continue
		}
		kept = append(kept, inst)
		al.selectionAgents.Store(inst.ID, inst)
		if !slices.Contains(al.unretired[inst.ID], inst) {
			al.unretired[inst.ID] = append(al.unretired[inst.ID], inst)
		}
	}
	return kept
}

// assignAll states the lists of instances in c, each from its own
// configuration.
func assignAll(c *models.Catalog, instances []*AgentInstance) {
	for _, inst := range instances {
		assignAgentModels(c, inst.builtWith, inst)
	}
}

// currentDigest is the digest of the configuration the running catalogue
// was built from.
func (al *AgentLoop) currentDigest() string {
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	return al.catalogDigest
}

// adoptFor builds the catalogue of cfg, states in it the lists of every
// agent the registry holds (each from its own configuration: rebuilt, kept
// busy, kept after a failed rebuild or temporary alike) and adopts it, so no
// agent loses its lists or its sessions' /model choices.
func (al *AgentLoop) adoptFor(registry *AgentRegistry, cfg *config.Config, digest string) {
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	next := al.catalogs.Build(cfg)
	instances := al.noteSelectionAgents(al.liveInstances(registry))
	assignAll(next, instances)
	al.adoptCatalog(next, digest)
}

// removeSet is told that instance inst is retired. While the registry has
// its id the set stays (the agent lives on in a newer instance). Otherwise,
// when another instance under that id is not retired yet (dropped while in
// a turn), the set is stated for the latest such one; when none is, the set
// is removed.
func (al *AgentLoop) removeSet(registry *AgentRegistry, inst *AgentInstance) {
	if al.removeSetDone != nil {
		defer al.removeSetDone(inst)
	}
	id := inst.ID
	// The bookkeeping first, then the registry: a retirement racing this one
	// sees inst gone whichever way this check goes.
	al.retired(inst)
	if _, ok := registry.Get(id); ok {
		// The hook runs after retired: TestRemoveSet_RetirementRacesDrop
		// stalls here and relies on that order.
		if al.removeSetHook != nil {
			al.removeSetHook(inst)
		}
		// A replaced instance: no list changes.
		return
	}
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	kept := al.retired(inst) // what is left now
	if _, ok := registry.Get(id); ok {
		return
	}
	if len(kept) > 0 {
		latest := kept[len(kept)-1]
		if v, ok := al.selectionAgents.Load(id); !ok || v != latest {
			al.selectionAgents.Store(id, latest)
			assignAgentModels(al.catalog, latest.builtWith, latest)
		}
	} else {
		al.selectionAgents.Delete(id)
		al.catalog.RemoveSet(id)
	}
}

// registryInstances are the instances the registry holds now.
func registryInstances(registry *AgentRegistry) []*AgentInstance {
	ids := registry.All()
	out := make([]*AgentInstance, 0, len(ids))
	for _, id := range ids {
		if inst, ok := registry.Get(id); ok && inst != nil {
			out = append(out, inst)
		}
	}
	return out
}

// summarizationNames is the agent's summarization chain as model names: its
// summarization_models, then the global summarization.models, de-duplicated.
// A name that names no enabled model is skipped (logCompressModel says so);
// nothing stands in for it. agentModelLists appends the agent's primary chat
// model.
func summarizationNames(cfg *config.Config, inst *AgentInstance) []string {
	var agentModels, globalModels []string
	if inst.Config != nil {
		agentModels = inst.Config.SummarizationModels
	}
	if cfg != nil {
		globalModels = cfg.Summarization.Models
	}
	var out []string
	for _, raw := range resolveCompressModelChain(agentModels, globalModels) {
		alias, _, ok := resolveCompressModelTarget(cfg, raw)
		if ok && !slices.Contains(out, alias) {
			out = append(out, alias)
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
			if strings.HasPrefix(r.Set, "\x00") {
				continue // a single-model set: no agent's choice
			}
			reverted = append(reverted, r.Set+"/"+string(r.Purpose)+": "+r.Model)
		}
		if len(reverted) > 0 {
			fields["selections_reverted"] = reverted
		}
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
		byKey[h.Key] = commands.CooldownEntry{
			Provider: h.Provider, Model: h.Model, Reason: string(h.Reason), Since: h.Since, Until: h.Until,
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
		if h.Provider == provider && h.Model == model {
			names = append(names, h.Name)
		}
	}
	cleared := false
	if len(names) > 0 {
		cleared = al.catalog.ResetHealth(names...) > 0
	}
	al.catalogs.Alerts().Rearm(spawnllm.ModelKey(provider, model))
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
	resp   *spawnllm.LLMResponse
	err    error
}

// option is the chain option that records each call, after which emit (when
// set) is called with it.
func (r *finishRecorder) option(emit func(c models.Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error)) models.CallOption {
	return models.WithFinish(func(c models.Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error) {
		r.called, r.resp, r.err = true, resp, err
		if emit != nil {
			emit(c, elapsed, resp, err)
		}
	})
}

// setCandidates are the models of agent id's set for purpose p (its own
// list, not one a fallback rule leads to), in priority order, with their
// providers and wire models.
func (al *AgentLoop) setCandidates(id string, p models.Purpose) []models.Candidate {
	return setCandidatesIn(al.catalog, id, p)
}

// setCandidatesIn is setCandidates in catalogue c.
func setCandidatesIn(c *models.Catalog, id string, p models.Purpose) []models.Candidate {
	list := c.Set(id).List(p)
	out := make([]models.Candidate, 0, len(list))
	for _, e := range list {
		if info, ok := c.Get(e.Name); ok {
			out = append(out, models.Candidate{
				Name: e.Name, Provider: info.Provider.Name, Protocol: info.Provider.Protocol, Model: info.Model.Model,
			})
		}
	}
	return out
}

// chatCandidates are agent a's chat models (setCandidates).
func (al *AgentLoop) chatCandidates(a *AgentInstance) []models.Candidate {
	return al.setCandidates(a.ID, models.Chat)
}

// workerCandidates are the models agent a's workers (sub-agents and Maestro
// dispatches) run on: its worker models when it has any configured (none
// when none of them is usable), else its chat models.
func (al *AgentLoop) workerCandidates(a *AgentInstance) []models.Candidate {
	if a.Subagents != nil && len(a.Subagents.Models) > 0 {
		return al.setCandidates(a.ID, models.Worker)
	}
	return al.chatCandidates(a)
}
