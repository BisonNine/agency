// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// The LLM provider clients, protocol DTOs, and the provider interfaces live in
// the agent SDK's package go.bison9.com/agent/spawnllm so ClawEh and Maestro
// can share the dispatch core (single binary) without import cycles. This file
// re-exports them under the historical providers names via aliases, so the
// policy/config code that stays here (dispatcher, factory, fallback, cooldown)
// and the rest of ClawEh compile unchanged. Add new code against either name —
// they are identical types.

package providers

import (
	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"
)

// MessageTypeToolError marks a tool result that reports a failure. Defined by
// the context engine, which reads it in its eviction sweep; re-exported here
// so the loop and the tools keep their historical name for it.
const MessageTypeToolError = ctxengine.MessageTypeToolError

// Identity names who a call is for (accounting.Identity); every Chat takes one.
type Identity = accounting.Identity

// Interfaces + concrete provider types.
type (
	LLMProvider            = spawnllm.LLMProvider
	StatefulProvider       = spawnllm.StatefulProvider
	ThinkingCapable        = spawnllm.ThinkingCapable
	CLIProvider            = spawnllm.CLIProvider
	HTTPProvider           = spawnllm.HTTPProvider
	ClaudeCliProvider      = spawnllm.ClaudeCliProvider
	CodexCliProvider       = spawnllm.CodexCliProvider
	AntigravityCliProvider = spawnllm.AntigravityCliProvider
	CursorCliProvider      = spawnllm.CursorCliProvider
	ModelConfig            = spawnllm.ModelConfig
)

// TextDeltaFunc is the token-streaming callback: a caller sets it under the
// TextDeltaOption key in the options map passed to provider.Chat to opt into
// per-delta assistant text streaming (honored by openai_compat/openai_responses;
// ignored elsewhere).
type TextDeltaFunc = spawnllm.TextDeltaFunc

// Protocol DTOs.
type (
	Message                = spawnllm.Message
	MessageAttachment      = spawnllm.MessageAttachment
	ToolCall               = spawnllm.ToolCall
	FunctionCall           = spawnllm.FunctionCall
	ToolDefinition         = spawnllm.ToolDefinition
	ToolFunctionDefinition = spawnllm.ToolFunctionDefinition
	LLMResponse            = spawnllm.LLMResponse
	GeneratedImage         = spawnllm.GeneratedImage
	UsageInfo              = spawnllm.UsageInfo
	DispatchStatus         = spawnllm.DispatchStatus
	ContentBlock           = spawnllm.ContentBlock
	CacheControl           = spawnllm.CacheControl
	ExtraContent           = spawnllm.ExtraContent
)

// Failover classification.
type (
	FailoverReason = spawnllm.FailoverReason
	FailoverError  = spawnllm.FailoverError
)

const (
	// TextDeltaOption is the options-map key for a providers.TextDeltaFunc; see
	// TextDeltaFunc above.
	TextDeltaOption = spawnllm.TextDeltaOption

	// ToolChoiceOption is the options-map key for the request's tool choice:
	// ToolChoiceNone keeps the tools declared but forbids calling them.
	ToolChoiceOption = spawnllm.ToolChoiceOption
	ToolChoiceNone   = spawnllm.ToolChoiceNone

	FailoverAuth         = spawnllm.FailoverAuth
	FailoverRateLimit    = spawnllm.FailoverRateLimit
	FailoverBilling      = spawnllm.FailoverBilling
	FailoverTimeout      = spawnllm.FailoverTimeout
	FailoverFormat       = spawnllm.FailoverFormat
	FailoverOverloaded   = spawnllm.FailoverOverloaded
	FailoverUnknown      = spawnllm.FailoverUnknown
	FailoverContextLimit = spawnllm.FailoverContextLimit
)

// Constructors + helpers.
var (
	NewHTTPProviderWithOptions           = spawnllm.NewHTTPProviderWithOptions
	NewUnconfiguredProvider              = spawnllm.NewUnconfiguredProvider
	NewClaudeCliProvider                 = spawnllm.NewClaudeCliProvider
	NewClaudeCliProviderWithTimeout      = spawnllm.NewClaudeCliProviderWithTimeout
	NewCodexCliProvider                  = spawnllm.NewCodexCliProvider
	NewCodexCliProviderWithTimeout       = spawnllm.NewCodexCliProviderWithTimeout
	NewAntigravityCliProvider            = spawnllm.NewAntigravityCliProvider
	NewAntigravityCliProviderWithTimeout = spawnllm.NewAntigravityCliProviderWithTimeout
	NewCursorCliProvider                 = spawnllm.NewCursorCliProvider
	NewCursorCliProviderWithTimeout      = spawnllm.NewCursorCliProviderWithTimeout

	ModelKey                = spawnllm.ModelKey
	splitModelKey           = spawnllm.SplitModelKey
	ClassifyError           = spawnllm.ClassifyError
	ReasonText              = spawnllm.ReasonText
	NormalizeToolCall       = spawnllm.NormalizeToolCall
	ParseModelRef           = spawnllm.ParseModelRef
	AgentIDFromContext      = spawnllm.AgentIDFromContext
	WithAgentID             = spawnllm.WithAgentID
	IsImageDimensionError   = spawnllm.IsImageDimensionError
	IsImageSizeError        = spawnllm.IsImageSizeError
	JSONObjectFortification = spawnllm.JSONObjectFortification
)

// The model catalogue's types under their historical names.
type (
	// FallbackCandidate is one model of a chain (Name is its model_name).
	FallbackCandidate = models.Candidate
	// FallbackAttempt is one model a call passed over (Name is its
	// model_name, Err its error).
	FallbackAttempt = models.Attempt
	// FallbackResult is a chain call's answer.
	FallbackResult = models.Result
	// FallbackExhaustedError is the error of a call that tried every model.
	FallbackExhaustedError = models.ExhaustedError
	// CLIDeclinedError is the error of a CLI that declined to use tools. Its
	// text is written for the user, so the failover renderer shows it
	// verbatim.
	CLIDeclinedError = models.CLIDeclinedError
)

// FallbackNotify is called when a chain moves past one or more models: the
// attempts just passed (one failure, or a run of cooldown skips) and the
// model tried next (models.WithNotify).
type FallbackNotify = func(passed []FallbackAttempt, next FallbackCandidate)
