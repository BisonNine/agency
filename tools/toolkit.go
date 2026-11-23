package tools

import (
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/agenttoken"
)

// toolStats tallies the tool calls of sub-agent runs, keyed by session key;
// a failed call's text is token-redacted before it is kept.
var toolStats = toolkit.NewTally(agenttoken.Redact)

var (
	// BeginToolStats starts counting the tool calls made under a session key.
	// Only sub-agent runs call it, so primary sessions are never tallied.
	BeginToolStats = toolStats.Begin
	// RecordToolResult counts one tool call made under a session key.
	RecordToolResult = toolStats.Record
	// EndToolStats stops counting a session key and returns its tally.
	EndToolStats = toolStats.End
)
