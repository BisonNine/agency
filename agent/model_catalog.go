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
		summarize := list(summarizationNames(cfg, inst), nil)
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

// reloadAssignments are the model assignments of the agents a reload
// builds. With next set (the reload changes what the catalogue is built
// from) they are made in next as each agent is built, and next is adopted at
// the commit. Otherwise they wait for the commit and are made then in the
// running catalogue, so a reload that fails changes no agent's lists.
type reloadAssignments struct {
	cfg  *config.Config
	next *models.Catalog

	mu    sync.Mutex
	built []*AgentInstance
}

// apply assigns the agents built, in c.
func (r *reloadAssignments) apply(c *models.Catalog) {
	r.mu.Lock()
	built := r.built
	r.built = nil
	r.mu.Unlock()
	for _, inst := range built {
		assignAgentModels(c, r.cfg, inst)
	}
}

// assignBuilt states the models of an agent just built: in the catalogue a
// reload in progress is building, at its commit (reloadAssignments), else in
// the running catalogue at once.
func (al *AgentLoop) assignBuilt(cfg *config.Config, inst *AgentInstance) {
	r := al.reloading.Load()
	switch {
	case r == nil:
		assignAgentModels(al.catalog, cfg, inst)
	case r.next != nil:
		assignAgentModels(r.next, cfg, inst)
	default:
		r.mu.Lock()
		r.built = append(r.built, inst)
		r.mu.Unlock()
	}
}

// summarizationNames is the agent's summarization chain as model names: its
// summarization_models, then the global summarization.models, de-duplicated,
// then its primary model unless already listed. A name that resolves to no
// enabled model is left out (the summarization caller logs it).
func summarizationNames(cfg *config.Config, inst *AgentInstance) []string {
	var agentModels, globalModels []string
	if inst.Config != nil {
		agentModels = inst.Config.SummarizationModels
	}
	if cfg != nil {
		globalModels = cfg.Summarization.Models
	}
	var out []string
	for _, raw := range append(resolveCompressModelChain(agentModels, globalModels), strings.TrimSpace(inst.Model)) {
		if alias, _, ok := resolveCompressModelTarget(cfg, raw); ok && !slices.Contains(out, alias) {
			out = append(out, alias)
		}
	}
	return out
}

// catalogAssignments is the part of a configuration the agents' model
// assignments are made from, for the catalogue digest: the agents' chat
// models, what their sets are built from. A reload that changes none of it,
// nor the providers and models, keeps the running catalogue and its
// cooldowns.
type catalogAssignments struct {
	DefaultModels []string           `json:"default_models"`
	Agents        []agentAssignments `json:"agents"`
}

type agentAssignments struct {
	ID     string   `json:"id"`
	Models []string `json:"models"`
}

func assignmentsOf(cfg *config.Config) catalogAssignments {
	a := catalogAssignments{DefaultModels: cfg.Agents.Defaults.Models}
	for i := range cfg.Agents.List {
		ac := &cfg.Agents.List[i]
		a.Agents = append(a.Agents, agentAssignments{ID: ac.ID, Models: ac.Models})
	}
	return a
}

// catalogDigest is the digest of the catalogue cfg builds (providers.Digest).
func catalogDigest(cfg *config.Config) string {
	return providers.Digest(cfg, assignmentsOf(cfg))
}

// adoptCatalog hands next to the running catalogue and logs what changed.
// Cooldowns are not carried over: every model starts available.
func (al *AgentLoop) adoptCatalog(next *models.Catalog, digest string) {
	diff := al.catalog.Adopt(next)
	al.catalogDigest = digest
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
