// ClawEh
// License: MIT

package agent

import (
	"maps"
	"reflect"
	"slices"
	"strings"
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
func agentModelLists(c *models.Catalog, cfg, current *config.Config, inst *AgentInstance) map[models.Purpose][]models.Assignment {
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
		summarize := list(summarizationNames(c, cfg, current, inst), nil)
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
	assignAgentModelsIn(c, cfg, cfg, inst)
}

// assignAgentModelsIn is assignAgentModels with the lists from cfg (the
// agent's own configuration) and any derived summarization model it adds
// defined by current (the configuration in force).
func assignAgentModelsIn(c *models.Catalog, cfg, current *config.Config, inst *AgentInstance) {
	if _, err := c.Set(inst.ID).ReplaceAll(agentModelLists(c, cfg, current, inst)); err != nil {
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
	// Before the lists: ReplaceAll saves kept choices through it.
	al.selectionAgents.Store(inst.ID, inst)
	assignAgentModelsIn(al.catalog, inst.builtWith, al.GetConfig(), inst)
}

// stateListsAfterCommit states every agent's lists after a reload
// committed, adopting a new catalogue when the digest changed, and then
// releases the listsGate the commit took, whatever happens.
func (al *AgentLoop) stateListsAfterCommit(registry *AgentRegistry, cfg *config.Config, digest string) {
	defer al.listsGate.Unlock()
	if digest != al.currentDigest() {
		al.adoptFor(registry, cfg, digest)
	} else {
		al.reconcileAll(registry, cfg)
	}
}

// reconcileAll is assignOne for every agent the registry holds, after the
// derived summarization models are brought in line with cfg (the
// configuration in force) and before the ones no list names any more are
// removed.
func (al *AgentLoop) reconcileAll(registry *AgentRegistry, cfg *config.Config) {
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	instances := registryInstances(registry)
	// Before the lists: ReplaceAll saves kept choices through the new
	// instances, not the replaced (closed) ones.
	al.noteSelectionAgents(instances)
	assignAll(al.catalog, cfg, instances)
}

// noteSelectionAgents records instances as the ones their sets' choices are
// read and written through.
func (al *AgentLoop) noteSelectionAgents(instances []*AgentInstance) {
	for _, inst := range instances {
		al.selectionAgents.Store(inst.ID, inst)
	}
}

// assignAll states the lists of instances in c, each from its own
// configuration, with the derived summarization models as cfg (the
// configuration in force) makes them and none left that no list names.
func assignAll(c *models.Catalog, cfg *config.Config, instances []*AgentInstance) {
	for _, inst := range instances {
		assignAgentModelsIn(c, inst.builtWith, cfg, inst)
	}
	// A derived model added while an older configuration was in force is
	// defined as that one made it: bring them in line, which drops a
	// replaced one from the lists, and state the lists again.
	if syncDerived(c, cfg) {
		for _, inst := range instances {
			assignAgentModelsIn(c, inst.builtWith, cfg, inst)
		}
	}
	pruneDerived(c, instances)
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
	instances := registryInstances(registry)
	al.noteSelectionAgents(instances)
	assignAll(next, cfg, instances)
	al.adoptCatalog(next, digest)
}

// removeSet drops the set of agent id, once the registry no longer has it,
// and the derived summarization models no remaining list names.
func (al *AgentLoop) removeSet(registry *AgentRegistry, id string) {
	al.assignMu.Lock()
	defer al.assignMu.Unlock()
	if _, ok := registry.Get(id); !ok {
		al.selectionAgents.Delete(id)
		al.catalog.RemoveSet(id)
		pruneDerived(al.catalog, registryInstances(registry))
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
// summarization_models, then the global summarization.models, de-duplicated,
// then its primary model unless already listed. A name that resolves to no
// enabled model is served as it always was, by the agents' default model
// with that name as its model id: a derived model (DerivedSummaryModel),
// added to c on first use, defined by current (the configuration in force).
// Without a default model it is left out.
func summarizationNames(c *models.Catalog, cfg, current *config.Config, inst *AgentInstance) []string {
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
		case raw != "" && current != nil && ensureDerived(c, current, raw):
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
	shown := func(names []string) []string {
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, providers.DisplayName(n))
		}
		return out
	}
	fields := map[string]any{}
	if len(diff.Added) > 0 {
		fields["added"] = shown(diff.Added)
	}
	if len(diff.Removed) > 0 {
		fields["removed"] = shown(diff.Removed)
	}
	if len(diff.Changed) > 0 {
		fields["changed"] = shown(diff.Changed)
	}
	if len(diff.Reverted) > 0 {
		reverted := make([]string, 0, len(diff.Reverted))
		for _, r := range diff.Reverted {
			if strings.HasPrefix(r.Set, "\x00") {
				continue // a single-model set: no agent's choice
			}
			reverted = append(reverted, r.Set+"/"+string(r.Purpose)+": "+providers.DisplayName(r.Model))
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

// ensureDerived adds to c the derived summarization model for raw
// (providers.DerivedSummaryModel) when c does not have it, and reports
// whether c has it. One c already has is brought in line with the
// configuration in force by syncDerived.
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

// syncDerived brings every derived summarization model in c in line with
// cfg: one whose definition differs from what the default model makes it
// now is replaced, so its client is built anew, as the default provider was
// rebuilt on every reload; one cfg can no longer derive (no default model)
// is removed. Their health stays: it is kept by provider and model id.
func syncDerived(c *models.Catalog, cfg *config.Config) (changed bool) {
	for _, name := range c.Names() {
		raw, ok := providers.DerivedRaw(name)
		if !ok {
			continue
		}
		want, ok := providers.DerivedSummaryModel(cfg, raw)
		info, _ := c.Get(name)
		if ok && reflect.DeepEqual(info.Model, want) {
			continue
		}
		if err := c.Remove(name); err != nil {
			continue
		}
		changed = true
		if ok {
			if err := c.Add(want); err != nil {
				logger.WarnCF("agent", "summarization model not replaced",
					map[string]any{"model": raw, "error": err.Error()})
			}
		}
	}
	return changed
}

// pruneDerived removes the derived summarization models no list of the
// given agents names any more.
func pruneDerived(c *models.Catalog, instances []*AgentInstance) {
	used := map[string]bool{}
	for _, inst := range instances {
		set := c.Set(inst.ID)
		for _, p := range []models.Purpose{models.Summarize, models.Memory} {
			for _, e := range set.List(p) {
				used[e.Name] = true
			}
		}
	}
	for _, name := range c.Names() {
		if _, derived := providers.DerivedRaw(name); derived && !used[name] {
			if err := c.Remove(name); err != nil {
				logger.DebugCF("agent", "unused summarization model not removed",
					map[string]any{"model": providers.DisplayName(name), "error": err.Error()})
			}
		}
	}
}
