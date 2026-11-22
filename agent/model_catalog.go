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

// assignAgentModels states in c which models agent inst uses for which
// purpose, in priority order (list position):
//   - Chat: its models (agents.list[].models, else the defaults');
//   - Worker: its worker models (subagents.models), when it has any; with
//     none the built-in Worker → Chat rule serves its sub-agents;
//   - Summarize and Memory: its summarization chain (agent list, then the
//     global one, then its primary model), the same list for both;
//   - one host purpose per task-model use, from TaskModelsConfig.Entries,
//     each entry carried as the assignment's settings.
//
// It changes nothing when the set already holds exactly that, so rebuilding
// an agent against an unchanged configuration leaves the set alone. A human
// agent gets its chat model only: no model ever sees a person's conversation.
func assignAgentModels(c *models.Catalog, cfg *config.Config, inst *AgentInstance) {
	set := c.Set(inst.ID)
	ensureAssigned(set, models.Chat, candidateNames(inst.Candidates), nil, inst.ID)
	var workers []string
	if inst.Subagents != nil && len(inst.Subagents.Models) > 0 {
		workers = candidateNames(inst.WorkerCandidates)
	}
	ensureAssigned(set, models.Worker, workers, nil, inst.ID)
	var summarize []string
	if inst.HumanModel == "" {
		summarize = summarizationNames(cfg, inst)
	}
	ensureAssigned(set, models.Summarize, summarize, nil, inst.ID)
	ensureAssigned(set, models.Memory, summarize, nil, inst.ID)
	for _, use := range taskUses {
		var names []string
		var settings []any
		if inst.HumanModel == "" && cfg != nil {
			for _, e := range cfg.TaskModels.Entries(use) {
				names = append(names, e.Model)
				settings = append(settings, e)
			}
		}
		ensureAssigned(set, taskPurpose(use), names, settings, inst.ID)
	}
}

// candidateNames are the model names of a chain.
func candidateNames(cands []providers.FallbackCandidate) []string {
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Name)
	}
	return names
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

// ensureAssigned makes purpose p of s hold exactly names, in that order,
// each with its settings (settings may be nil, or one per name).
func ensureAssigned(s *models.Set, p models.Purpose, names []string, settings []any, agentID string) {
	cur := s.List(p)
	have := make([]string, 0, len(cur))
	haveSettings := make([]any, 0, len(cur))
	for _, e := range cur {
		have = append(have, e.Name)
		haveSettings = append(haveSettings, e.Settings)
	}
	if slices.Equal(have, names) && (settings == nil || reflect.DeepEqual(haveSettings, settings)) {
		return
	}
	for _, n := range have {
		s.Unassign(p, n)
	}
	for i, n := range names {
		var opts []models.AssignOption
		if settings != nil {
			opts = append(opts, models.WithSettings(settings[i]))
		}
		if err := s.Assign(p, n, i, opts...); err != nil {
			// The catalogue left the model out (and logged why), or the
			// list names it twice.
			logger.DebugCF("agent", "model not assigned",
				map[string]any{"agent_id": agentID, "purpose": string(p), "model": n, "error": err.Error()})
		}
	}
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
// wire model (models that share both share their health), ordered by them:
// the catalogue's and the task models' tracker's.
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
	if tracker := al.cooldownTracker(); tracker != nil {
		for _, s := range tracker.Snapshot() {
			key := providers.ModelKey(s.Provider, s.Model)
			if cur, ok := byKey[key]; ok && !cur.Until.Before(s.Until) {
				continue
			}
			byKey[key] = commands.CooldownEntry{
				Provider: s.Provider, Model: s.Model, Reason: string(s.Reason), Since: s.Since, Until: s.Until,
			}
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
	if tracker := al.cooldownTracker(); tracker != nil && tracker.Clear(provider, model) {
		cleared = true
	}
	return cleared
}

// clearCooldowns clears the health of every model and re-arms every model's
// alerts.
func (al *AgentLoop) clearCooldowns() {
	al.catalog.ResetHealth()
	al.catalogs.Alerts().RearmAll()
	if tracker := al.cooldownTracker(); tracker != nil {
		tracker.Reset()
	}
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
