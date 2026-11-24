// Agency
// License: MIT

package report

import (
	"testing"
)

// noRow fails the test when the assessment has a row named item.
func noRow(t *testing.T, s Section, item string) {
	t.Helper()
	for _, r := range s.Tables[0].Rows {
		if r[1] == item {
			t.Fatalf("unexpected row %v", r)
		}
	}
}

// TestAssessment_AgentWithoutModel: an agent whose models (its own, else
// the default models) name no enabled model in the model list is marked,
// named, for an unknown name, a disabled model and an empty entry alike; an
// agent with one usable model, or inheriting usable default models, is not.
func TestAssessment_AgentWithoutModel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []string
		status string
	}{
		{"unknown", []string{"no-such-model"}, "No usable model: its models are not in the model list or disabled."},
		{"disabled", []string{"GPT Old"}, "No usable model: its models are not in the model list or disabled."},
		{"empty entry", []string{""}, "No usable model: its model list is empty."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, env := fixtureConfig(t)
			cfg.Agents.List[1].Models = tc.models // bob
			s := collectAssessment(t.Context(), cfg, env)
			r := assessmentRow(t, s, "Models (Bob)")
			if r[0] == "" {
				t.Error("expected an action mark")
			}
			if r[2] != tc.status {
				t.Errorf("status = %q, want %q", r[2], tc.status)
			}
			noRow(t, s, "Models (Alice)") // the default models
		})
	}

	cfg, env := fixtureConfig(t)
	cfg.Agents.List[1].Models = []string{"GPT Old", "gpt-5.5"} // one by its model id
	noRow(t, collectAssessment(t.Context(), cfg, env), "Models (Bob)")

	// The default models are what an agent without a list of its own uses.
	cfg.Agents.Defaults.Models = []string{"GPT Old"}
	s := collectAssessment(t.Context(), cfg, env)
	if r := assessmentRow(t, s, "Models (Alice)"); r[0] == "" {
		t.Error("expected an action mark for Alice on the disabled default model")
	}
	noRow(t, s, "Models (Bob)")
}

// TestAssessment_MemoryModelSkipped: a memory model (global or an agent's
// own) that is not an enabled model in the model list is marked and named,
// as skipped; a usable one, by name or model id, is not.
func TestAssessment_MemoryModelSkipped(t *testing.T) {
	cfg, env := fixtureConfig(t)
	cfg.Summarization.Models = []string{"GPT", "raw-summarizer"}
	cfg.Agents.List[0].SummarizationModels = []string{"gpt-5.5", "GPT Old", ""}
	s := collectAssessment(t.Context(), cfg, env)
	r := assessmentRow(t, s, "Memory models")
	if r[0] == "" || r[2] != "Not in the model list or disabled, so skipped: raw-summarizer." {
		t.Errorf("row = %v", r)
	}
	r = assessmentRow(t, s, "Memory models (Alice)")
	if r[0] == "" || r[2] != "Not in the model list or disabled, so skipped: GPT Old." {
		t.Errorf("row = %v", r)
	}
	noRow(t, s, "Memory models (Bob)")

	cfg.Summarization.Models = []string{"GPT"}
	cfg.Agents.List[0].SummarizationModels = nil
	s = collectAssessment(t.Context(), cfg, env)
	noRow(t, s, "Memory models")
	noRow(t, s, "Memory models (Alice)")
}
