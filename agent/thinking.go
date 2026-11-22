package agent

import "go.bison9.com/agent/modelspec"

// ThinkingLevel controls how the provider sends thinking parameters
// (modelspec.ThinkingLevel).
type ThinkingLevel = modelspec.ThinkingLevel

// Thinking levels.
const (
	ThinkingOff      = modelspec.ThinkingOff
	ThinkingLow      = modelspec.ThinkingLow
	ThinkingMedium   = modelspec.ThinkingMedium
	ThinkingHigh     = modelspec.ThinkingHigh
	ThinkingXHigh    = modelspec.ThinkingXHigh
	ThinkingMax      = modelspec.ThinkingMax
	ThinkingAdaptive = modelspec.ThinkingAdaptive
)
