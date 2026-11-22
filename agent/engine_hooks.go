// ClawEh
// License: MIT

package agent

// This file is part of the turn engine (the engine_*.go files): it imports no
// host package and uses no host identifier, so phase 5c can move it to the
// SDK unchanged. engine_guard_test.go enforces that.

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// turnHooks are a turn's callbacks and event consumers, each kind in the
// order it was registered. The host builds one per turn (newTurnHooks) and
// the engine calls it; the engine never reaches host behaviour any other way
// than through these and turnEnv.
type turnHooks struct {
	beforeTurn   []hook.BeforeTurnFunc
	afterTurn    []hook.AfterTurnFunc
	beforeModel  []hook.BeforeModelFunc
	afterModel   []hook.AfterModelFunc
	onModelError []hook.OnModelErrorFunc
	onFallback   []hook.OnFallbackFunc
	beforeTool   []hook.BeforeToolFunc
	afterTool    []hook.AfterToolFunc
	onToolError  []hook.OnToolErrorFunc
	onCompaction []hook.OnCompactionFunc
	onTextDelta  []hook.OnTextDeltaFunc
	onInterrupt  []hook.OnInterruptFunc
	onEvent      []hook.OnEventFunc

	// interim are the steps the hook package has no type for yet; see
	// interimHooks.
	interim []interimHooks

	// order records every registration as "Kind:plugin", in order, so a test
	// can pin the order the host registers in.
	order []string
	// log receives the panics recovered from callbacks and consumers.
	log *slog.Logger
}

// interimHooks are host steps the hook package has no callback or event for
// yet. Each keeps a behaviour exactly where it happens today; each is a
// requested SDK addition (see the phase 5a report) and goes when that lands.
type interimHooks struct {
	Name string
	// Narrate is told the text a model sent with its tool calls, once the
	// batch passed the repeated-batch check and before the tool-call message
	// is stored.
	Narrate func(ctx context.Context, iteration int, content string)
	// ToolsReturned is told that every call of a batch returned, before the
	// first result is stored.
	ToolsReturned func(ctx context.Context, calls int)
	// ToolResult is told each result of a batch in call order, just before
	// it is stored.
	ToolResult func(ctx context.Context, call hook.ToolCall, result *toolkit.Result)
	// DescribeImages describes the images a batch's tools returned for a
	// model that cannot see them (vision off), once the batch's results are
	// stored. It returns the user message to store and whether a
	// description was produced; add false stores nothing.
	DescribeImages func(ctx context.Context, images, focus []string) (content string, described, add bool)
}

// add registers p's callbacks, each at the end of its kind.
func (h *turnHooks) add(p hook.Plugin) {
	reg := func(kind string, set bool) {
		if set {
			h.order = append(h.order, kind+":"+p.Name)
		}
	}
	if p.BeforeTurn != nil {
		h.beforeTurn = append(h.beforeTurn, p.BeforeTurn)
	}
	reg("BeforeTurn", p.BeforeTurn != nil)
	if p.AfterTurn != nil {
		h.afterTurn = append(h.afterTurn, p.AfterTurn)
	}
	reg("AfterTurn", p.AfterTurn != nil)
	if p.BeforeModel != nil {
		h.beforeModel = append(h.beforeModel, p.BeforeModel)
	}
	reg("BeforeModel", p.BeforeModel != nil)
	if p.AfterModel != nil {
		h.afterModel = append(h.afterModel, p.AfterModel)
	}
	reg("AfterModel", p.AfterModel != nil)
	if p.OnModelError != nil {
		h.onModelError = append(h.onModelError, p.OnModelError)
	}
	reg("OnModelError", p.OnModelError != nil)
	if p.OnFallback != nil {
		h.onFallback = append(h.onFallback, p.OnFallback)
	}
	reg("OnFallback", p.OnFallback != nil)
	if p.BeforeTool != nil {
		h.beforeTool = append(h.beforeTool, p.BeforeTool)
	}
	reg("BeforeTool", p.BeforeTool != nil)
	if p.AfterTool != nil {
		h.afterTool = append(h.afterTool, p.AfterTool)
	}
	reg("AfterTool", p.AfterTool != nil)
	if p.OnToolError != nil {
		h.onToolError = append(h.onToolError, p.OnToolError)
	}
	reg("OnToolError", p.OnToolError != nil)
	if p.OnCompaction != nil {
		h.onCompaction = append(h.onCompaction, p.OnCompaction)
	}
	reg("OnCompaction", p.OnCompaction != nil)
	if p.OnTextDelta != nil {
		h.onTextDelta = append(h.onTextDelta, p.OnTextDelta)
	}
	reg("OnTextDelta", p.OnTextDelta != nil)
	if p.OnInterrupt != nil {
		h.onInterrupt = append(h.onInterrupt, p.OnInterrupt)
	}
	reg("OnInterrupt", p.OnInterrupt != nil)
	if p.OnEvent != nil {
		h.onEvent = append(h.onEvent, p.OnEvent)
	}
	reg("OnEvent", p.OnEvent != nil)
}

// addInterim registers interim host steps (interimHooks), after those
// already registered.
func (h *turnHooks) addInterim(i interimHooks) {
	h.interim = append(h.interim, i)
	for _, k := range []struct {
		kind string
		set  bool
	}{
		{"Narrate", i.Narrate != nil},
		{"ToolsReturned", i.ToolsReturned != nil},
		{"ToolResult", i.ToolResult != nil},
		{"DescribeImages", i.DescribeImages != nil},
	} {
		if k.set {
			h.order = append(h.order, "Interim."+k.kind+":"+i.Name)
		}
	}
}

// recovered logs a panic recovered from a callback or consumer; the turn goes
// on as if the callback had returned nothing (hook's callback rules).
func (h *turnHooks) recovered(kind string, r any) {
	if h == nil || h.log == nil {
		return
	}
	h.log.Error("panic in turn callback", "component", "agent", "callback", kind,
		"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
}

// emit delivers ev to every OnEvent consumer, in order, synchronously.
func (h *turnHooks) emit(ctx context.Context, ev hook.Event) {
	if h == nil {
		return
	}
	for _, fn := range h.onEvent {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("OnEvent", r)
				}
			}()
			fn(ctx, ev)
		}()
	}
}

// callAfterModel runs the AfterModel callbacks on resp.
func (h *turnHooks) callAfterModel(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) {
	for _, fn := range h.afterModel {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("AfterModel", r)
				}
			}()
			fn(ctx, call, resp)
		}()
	}
}

// callOnFallback runs the OnFallback taps.
func (h *turnHooks) callOnFallback(ctx context.Context, ev hook.Fallback) {
	for _, fn := range h.onFallback {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("OnFallback", r)
				}
			}()
			fn(ctx, ev)
		}()
	}
}

// callOnCompaction runs the OnCompaction taps.
func (h *turnHooks) callOnCompaction(ctx context.Context, ev hook.Compaction) {
	for _, fn := range h.onCompaction {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("OnCompaction", r)
				}
			}()
			fn(ctx, ev)
		}()
	}
}

// callOnTextDelta runs the OnTextDelta taps.
func (h *turnHooks) callOnTextDelta(ctx context.Context, ev hook.TextDelta) {
	for _, fn := range h.onTextDelta {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("OnTextDelta", r)
				}
			}()
			fn(ctx, ev)
		}()
	}
}

// eachInterim runs fn for every registered interim set, recovering a panic.
func (h *turnHooks) eachInterim(kind string, fn func(i interimHooks)) {
	for _, i := range h.interim {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered("Interim."+kind, r)
				}
			}()
			fn(i)
		}()
	}
}

// turnOnContext is what a turn puts on its context: its hooks and scope, so
// a step reached through another package (the context engine's compaction
// start) can call back into the turn it runs for.
type turnOnContext struct {
	hooks *turnHooks
	clock clock.Clock
	// live is set from TurnStarted to TurnFinished: a step reported outside
	// it (a compaction while the host stores the turn's message) reaches the
	// callbacks but emits no event, so TurnStarted stays the first event and
	// TurnFinished the last.
	live atomic.Bool
}

type turnOnContextKey struct{}

// beginTurnContext returns ctx carrying the turn's scope (hook.WithScope,
// which also sets toolkit's agent id and session key) and its hooks. The host
// calls it once per turn, before anything of the turn reaches the context
// engine.
func beginTurnContext(ctx context.Context, scope hook.Scope, hooks *turnHooks, now clock.Clock) context.Context {
	ctx = hook.WithScope(ctx, scope)
	return context.WithValue(ctx, turnOnContextKey{}, &turnOnContext{hooks: hooks, clock: now})
}

// turnHooksOf returns the hooks of the turn ctx belongs to, nil outside one.
func turnHooksOf(ctx context.Context) (*turnOnContext, bool) {
	t, ok := ctx.Value(turnOnContextKey{}).(*turnOnContext)
	return t, ok && t != nil && t.hooks != nil
}

// engineCompactionStart is the context engine's compaction-start hook
// (ctxengine.WithCompactionStartFunc): it reports the pass to the turn the
// context belongs to, as OnCompaction and a Compaction event (phase started),
// before the summary model is called. A pass with no turn on its context (a
// background pass) reaches no turn.
func engineCompactionStart(ctx context.Context, info ctxengine.CompactionStart) {
	t, ok := turnHooksOf(ctx)
	if !ok {
		return
	}
	scope, _ := hook.ScopeOf(ctx)
	scope.Session = info.SessionKey
	header := hook.EventHeader{Scope: scope, At: t.clock.Now()}
	ev := hook.Compaction{
		EventHeader: header,
		Phase:       hook.CompactionStarted,
		Reason:      string(info.Reason),
		Blocking:    info.Blocking,
	}
	t.hooks.callOnCompaction(ctx, ev)
	if t.live.Load() {
		t.hooks.emit(ctx, ev)
	}
}
