// ClawEh
// License: MIT

package report

import (
	"strings"

	"go.bison9.com/agency/config"
)

// usableModel reports whether ref, an entry of a model list, names a model
// in the model list that is enabled: by model_name, or by its model id. This
// is how the agent resolves the entries of its lists (agent.modelListLookup);
// an entry that does not resolve is skipped, and an empty one never does.
func usableModel(cfg *config.Config, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	for i := range cfg.Models {
		m := &cfg.Models[i]
		if !m.Enabled {
			continue
		}
		wire := strings.TrimSpace(m.Model)
		if (m.ModelName == ref && wire != "") || wire == ref {
			return true
		}
	}
	return false
}

// unusableModels are the non-empty entries of refs that name no usable
// model, in order.
func unusableModels(cfg *config.Config, refs []string) []string {
	var out []string
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" && !usableModel(cfg, r) {
			out = append(out, r)
		}
	}
	return out
}

// skippedText is the status of a memory model row: the entries skipped.
func skippedText(names []string) string {
	return "Not in the model list or disabled, so skipped: " + strings.Join(names, ", ") + "."
}

// agentModelAssessment is the assessment rows for model lists an agent does
// not run as configured, all marked:
//   - an enabled agent (not one representing a person) whose models (its own,
//     else the default models) name no usable model: its turns fail, it does
//     not run on another model;
//   - the global memory models and each such agent's own that name no usable
//     model: those entries are skipped (the agent's first model serves).
func agentModelAssessment(cfg *config.Config) []assessRow {
	var rows []assessRow
	if skipped := unusableModels(cfg, cfg.Summarization.Models); len(skipped) > 0 {
		rows = append(rows, assessRow{true, "Memory models", skippedText(skipped)})
	}
	for _, a := range enabledAgents(cfg) {
		if cfg.IsHumanAgent(a.ID) {
			continue
		}
		name := a.DisplayName()
		list := a.Models
		if len(list) == 0 {
			list = cfg.Agents.Defaults.Models
		}
		usable, named := false, false
		for _, ref := range list {
			usable = usable || usableModel(cfg, ref)
			named = named || strings.TrimSpace(ref) != ""
		}
		switch {
		case !named:
			rows = append(rows, assessRow{true, "Models (" + name + ")", "No usable model: its model list is empty."})
		case !usable:
			rows = append(rows, assessRow{true, "Models (" + name + ")", "No usable model: its models are not in the model list or disabled."})
		}
		if skipped := unusableModels(cfg, a.SummarizationModels); len(skipped) > 0 {
			rows = append(rows, assessRow{true, "Memory models (" + name + ")", skippedText(skipped)})
		}
	}
	return rows
}
