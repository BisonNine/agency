package tools

import (
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/agenttoken"
)

// The tool core is the SDK's toolkit package. These aliases keep the names
// the tool packages have always used, so they need no edits.

type (
	Tool          = toolkit.Tool
	AsyncCallback = toolkit.AsyncCallback
	AsyncExecutor = toolkit.AsyncExecutor
	SessionScoped = toolkit.SessionScoped
	ExternalNamer = toolkit.ExternalNamer
	ToolResult    = toolkit.Result
)

var (
	NewToolResult        = toolkit.NewResult
	SilentResult         = toolkit.SilentResult
	AsyncResult          = toolkit.AsyncResult
	ErrorResult          = toolkit.ErrorResult
	UserResult           = toolkit.UserResult
	MediaResult          = toolkit.MediaResult
	NotEnabledMessage    = toolkit.NotEnabledMessage
	Refusal              = toolkit.Refusal
	IsExpectedRefusal    = toolkit.IsExpectedRefusal
	ToolToSchema         = toolkit.ToolToSchema
	OpenObjectProperties = toolkit.OpenObjectProperties
	ResultToGlobal       = toolkit.ResultToSpec

	WithToolContext = toolkit.WithChat
	ToolChannel     = toolkit.Channel
	ToolChatID      = toolkit.ChatID
	WithSessionKey  = toolkit.WithSessionKey
	ToolSessionKey  = toolkit.SessionKey

	ErrToolNotRegistered = toolkit.ErrToolNotRegistered
	ErrToolSessionScoped = toolkit.ErrToolSessionScoped
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
