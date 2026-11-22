// ClawEh
// License: MIT

package agent

import (
	"encoding/json"

	"go.bison9.com/agent/ctxengine/memory"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/utils"
)

// buildSessionInfo constructs session info for the given agent and session key.
// Extracted so it can be reused by registerAgentTools in loop_tools.go.
func buildSessionInfo(al *AgentLoop, agent *AgentInstance, sessionKey string) (*tools.SessionInfo, error) {
	info := &tools.SessionInfo{
		SessionKey: sessionKey,
	}

	cm, release := al.getContextManager(agent, sessionKey)
	defer release()
	stats := cm.Stats()
	info.ContextMessageCount = stats.TotalMessages
	if !stats.LastCompressedAt.IsZero() {
		t := stats.LastCompressedAt
		info.LastCompressedAt = &t
	}

	rawSummary := agent.Sessions.GetSummary(sessionKey)
	if rawSummary != "" {
		var sv struct {
			CoveredSeqStart int64 `json:"covered_seq_start"`
			CoveredSeqEnd   int64 `json:"covered_seq_end"`
		}
		if err := json.Unmarshal([]byte(rawSummary), &sv); err == nil && sv.CoveredSeqStart > 0 {
			info.SummaryCovers = &tools.SummaryCoverage{
				SeqStart: sv.CoveredSeqStart,
				SeqEnd:   sv.CoveredSeqEnd,
			}
			if !stats.LastCompressedAt.IsZero() {
				t := stats.LastCompressedAt
				info.SummaryCovers.GeneratedAt = &t
			}
		}
	}

	archivePath := archiveDBPath(agent.StateDir, sessionKey)
	if a, openErr := memory.OpenReadOnly(archivePath, logger.SDK()); openErr == nil {
		defer utils.CloseQuietly(a)
		minSeq, maxSeq, boundsErr := a.Bounds()
		if boundsErr == nil {
			info.ArchiveMinSeq = minSeq
			info.ArchiveMaxSeq = maxSeq
			if maxSeq >= minSeq && minSeq > 0 {
				info.TotalArchived = maxSeq - minSeq + 1
			}
			if minSeq > 0 {
				if startMsgs, err := a.QueryRange(minSeq, minSeq); err == nil && len(startMsgs) > 0 {
					t := startMsgs[0].CreatedAt
					info.StartedAt = &t
				}
			}
		}
	}

	return info, nil
}
