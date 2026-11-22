package tools

import (
	"context"
	"sync/atomic"
)

// ToolAllowChecker is implemented by any type that can determine whether a
// named tool is permitted. config.AgentConfig satisfies this interface.
// Keeping it here avoids importing config into tools.
//
// IsToolAllowed is the full per-tool decision (allow list minus deny list).
// IsToolDenied is the deny-only half, applied even to suite tools that are
// exempt from the allow list, so an explicit deny wins however a tool arrived.
type ToolAllowChecker interface {
	IsToolAllowed(name string) bool
	IsToolDenied(name string) bool
}

// --- Request-scoped tool context (allow checker, round-sent flag) ---
//
// Carried via context.Value so that concurrent tool calls each receive
// their own immutable copy — no mutable state on singleton tool instances.
//
// Keys are unexported pointer-typed vars — guaranteed collision-free,
// and only accessible through the helper functions below.

type toolCtxKey struct{ name string }

var (
	ctxKeyAllowChecker = &toolCtxKey{"allowChecker"}
	ctxKeyRoundSent    = &toolCtxKey{"roundSent"}
)

// WithToolAllowChecker returns a child context carrying an allow checker.
// This is used by ExecuteWithContext for defense-in-depth enforcement.
func WithToolAllowChecker(ctx context.Context, checker ToolAllowChecker) context.Context {
	return context.WithValue(ctx, ctxKeyAllowChecker, checker)
}

// ToolAllowCheckerFromCtx extracts the ToolAllowChecker from ctx, or nil if unset.
func ToolAllowCheckerFromCtx(ctx context.Context) ToolAllowChecker {
	if v, ok := ctx.Value(ctxKeyAllowChecker).(ToolAllowChecker); ok {
		return v
	}
	return nil
}

// WithRoundSentFlag returns a child context carrying a per-round sent flag.
// Used by the concurrent agent loop to track whether the message tool fired.
func WithRoundSentFlag(ctx context.Context, flag *atomic.Bool) context.Context {
	return context.WithValue(ctx, ctxKeyRoundSent, flag)
}

// roundSentFlagFromCtx extracts the per-round sent flag, or nil if not set.
func roundSentFlagFromCtx(ctx context.Context) *atomic.Bool {
	if v, ok := ctx.Value(ctxKeyRoundSent).(*atomic.Bool); ok {
		return v
	}
	return nil
}

// RoundSentFlagFromCtx is the exported form of roundSentFlagFromCtx,
// available to sub-packages (e.g. tools/msg).
func RoundSentFlagFromCtx(ctx context.Context) *atomic.Bool {
	return roundSentFlagFromCtx(ctx)
}
