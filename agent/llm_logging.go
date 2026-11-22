// ClawEh
// License: MIT

package agent

import (
	"encoding/json"
	"time"

	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// dumpRefusal writes a diagnostic file capturing the full LLM input and output
// when the provider returns finish_reason "refusal". Called only when
// cfg.Logging.DumpRefusals is true and dumpsDir is set.
func (al *AgentLoop) dumpRefusal(
	agent *AgentInstance,
	messages []providers.Message,
	response *providers.LLMResponse,
	opts processOptions,
	model string,
	iteration int,
) {
	inputBytes, err := json.Marshal(messages)
	if err != nil {
		logger.WarnCF("agent", "Failed to marshal refusal dump input",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}
	outputBytes, err := json.Marshal(response)
	if err != nil {
		logger.WarnCF("agent", "Failed to marshal refusal dump output",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}

	meta := map[string]any{
		"agent":     agent.ID,
		"model":     model,
		"session":   opts.SessionKey,
		"channel":   opts.Channel,
		"iteration": iteration,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	basename, err := dump.Write(al.dumpsDir, "refusal", meta, json.RawMessage(inputBytes), json.RawMessage(outputBytes))
	if err != nil {
		logger.WarnCF("agent", "Failed to write refusal dump",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}
	logger.WarnCF("agent", "LLM refusal detected — dump written",
		map[string]any{
			"agent_id":    agent.ID,
			"model":       model,
			"session_key": opts.SessionKey,
			"channel":     opts.Channel,
			"iteration":   iteration,
			"dump_base":   basename,
		})
}

// dumpAll writes a diagnostic file capturing the full LLM input and output
// for every response when cfg.Logging.DumpAll is true and dumpsDir is set.
func (al *AgentLoop) dumpAll(
	agent *AgentInstance,
	messages []providers.Message,
	response *providers.LLMResponse,
	opts processOptions,
	model string,
	iteration int,
) {
	inputBytes, err := json.Marshal(messages)
	if err != nil {
		logger.WarnCF("agent", "Failed to marshal dump_all input",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}
	outputBytes, err := json.Marshal(response)
	if err != nil {
		logger.WarnCF("agent", "Failed to marshal dump_all output",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}

	meta := map[string]any{
		"agent":         agent.ID,
		"model":         model,
		"session":       opts.SessionKey,
		"channel":       opts.Channel,
		"iteration":     iteration,
		"finish_reason": response.FinishReason,
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	basename, err := dump.Write(al.dumpsDir, "dump_all", meta, json.RawMessage(inputBytes), json.RawMessage(outputBytes))
	if err != nil {
		logger.WarnCF("agent", "Failed to write dump_all file",
			map[string]any{"agent_id": agent.ID, "error": err.Error()})
		return
	}
	logger.DebugCF("agent", "LLM response dump written (dump_all)",
		map[string]any{
			"agent_id":      agent.ID,
			"model":         model,
			"session_key":   opts.SessionKey,
			"finish_reason": response.FinishReason,
			"iteration":     iteration,
			"dump_base":     basename,
		})
}
