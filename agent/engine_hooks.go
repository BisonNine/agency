// ClawEh
// License: MIT

package agent

// This file is part of the turn engine (the engine_*.go files): it imports no
// host package and uses no host identifier, so it can live in a package of
// its own. engine_guard_test.go enforces that.

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
//
// Panics: a callback (AfterModel, OnFallback, OnCompaction, OnTextDelta, the
// tool callbacks, the interim steps) changes a step or produces the turn's
// state, so a panic in one is not recovered: it ends the turn, as a panic in
// the loop always has. An event consumer only watches, so a panic in one is
// recovered and logged at error level with the consumer's name and stack,
// and the turn goes on.
type turnHooks struct {
	afterModel   []hook.AfterModelFunc
	onFallback   []hook.OnFallbackFunc
	beforeTool   []hook.BeforeToolFunc
	afterTool    []hook.AfterToolFunc
	onToolError  []hook.OnToolErrorFunc
	onCompaction []hook.OnCompactionFunc
	onTextDelta  []hook.OnTextDeltaFunc
	onEvent      []namedConsumer

	// interim are the steps the hook package has no type for; see
	// interimHooks.
	interim []interimHooks

	// log receives the panics recovered from event consumers.
	log *slog.Logger
}

// namedConsumer is an event consumer with the name of the plugin that
// registered it, for the log of a recovered panic.
type namedConsumer struct {
	name string
	fn   hook.OnEventFunc
}

// interimHooks are host steps the hook package has no callback or event for.
// Each keeps a behaviour exactly where it happens in the loop; a hook type
// that carries it replaces it.
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

// add registers p's callbacks, each at the end of its kind. A callback of a
// kind the engine does not call yet (BeforeTurn, AfterTurn, BeforeModel,
// OnModelError, OnInterrupt) is refused with a panic, so none is ever
// registered and silently ignored.
func (h *turnHooks) add(p hook.Plugin) {
	for _, k := range []struct {
		kind string
		set  bool
	}{
		{"BeforeTurn", p.BeforeTurn != nil},
		{"AfterTurn", p.AfterTurn != nil},
		{"BeforeModel", p.BeforeModel != nil},
		{"OnModelError", p.OnModelError != nil},
		{"OnInterrupt", p.OnInterrupt != nil},
	} {
		if k.set {
			panic(fmt.Sprintf("turn engine: plugin %q registers a %s callback, which the engine does not call", p.Name, k.kind))
		}
	}
	if p.AfterModel != nil {
		h.afterModel = append(h.afterModel, p.AfterModel)
	}
	if p.OnFallback != nil {
		h.onFallback = append(h.onFallback, p.OnFallback)
	}
	if p.BeforeTool != nil {
		h.beforeTool = append(h.beforeTool, p.BeforeTool)
	}
	if p.AfterTool != nil {
		h.afterTool = append(h.afterTool, p.AfterTool)
	}
	if p.OnToolError != nil {
		h.onToolError = append(h.onToolError, p.OnToolError)
	}
	if p.OnCompaction != nil {
		h.onCompaction = append(h.onCompaction, p.OnCompaction)
	}
	if p.OnTextDelta != nil {
		h.onTextDelta = append(h.onTextDelta, p.OnTextDelta)
	}
	if p.OnEvent != nil {
		h.onEvent = append(h.onEvent, namedConsumer{name: p.Name, fn: p.OnEvent})
	}
}

// addInterim registers interim host steps (interimHooks), after those
// already registered.
func (h *turnHooks) addInterim(i interimHooks) {
	h.interim = append(h.interim, i)
}

// emit delivers ev to every OnEvent consumer, in order, synchronously. A
// consumer that panics is logged and skipped.
func (h *turnHooks) emit(ctx context.Context, ev hook.Event) {
	if h == nil {
		return
	}
	for _, c := range h.onEvent {
		h.deliver(ctx, c, ev)
	}
}

func (h *turnHooks) deliver(ctx context.Context, c namedConsumer, ev hook.Event) {
	defer func() {
		if r := recover(); r != nil && h.log != nil {
			h.log.Error("panic in turn event consumer", "component", "agent", "consumer", c.name,
				"event", fmt.Sprintf("%T", ev), "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()
	c.fn(ctx, ev)
}

// callAfterModel runs the AfterModel callbacks on resp. A callback that sets
// resp.Response to nil changes nothing: the response it was given stays.
func (h *turnHooks) callAfterModel(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) {
	for _, fn := range h.afterModel {
		given := resp.Response
		fn(ctx, call, resp)
		if resp.Response == nil {
			resp.Response = given
		}
	}
}

// callOnFallback runs the OnFallback taps.
func (h *turnHooks) callOnFallback(ctx context.Context, ev hook.Fallback) {
	for _, fn := range h.onFallback {
		fn(ctx, ev)
	}
}

// callOnCompaction runs the OnCompaction taps.
func (h *turnHooks) callOnCompaction(ctx context.Context, ev hook.Compaction) {
	for _, fn := range h.onCompaction {
		fn(ctx, ev)
	}
}

// callOnTextDelta runs the OnTextDelta taps.
func (h *turnHooks) callOnTextDelta(ctx context.Context, ev hook.TextDelta) {
	for _, fn := range h.onTextDelta {
		fn(ctx, ev)
	}
}

// eachInterim runs fn for every registered interim set, in order.
func (h *turnHooks) eachInterim(fn func(i interimHooks)) {
	for _, i := range h.interim {
		fn(i)
	}
}

// turnOnContext is what a turn puts on its context: its hooks and clock, so
// a step reached through another package (the context engine's compaction
// start) can call back into the turn it runs for. The hooks may be put in
// place after the context is made (use), so a host can scope the turn before
// its model loop is set up.
type turnOnContext struct {
	hooks atomic.Pointer[turnHooks]
	clock clock.Clock
	// live is set from TurnStarted to TurnFinished: a step reported outside
	// it (a compaction while the host stores the turn's message) reaches the
	// callbacks but emits no event, so TurnStarted stays the first event and
	// TurnFinished the last.
	live atomic.Bool
}

// use makes h the turn's hooks.
func (t *turnOnContext) use(h *turnHooks) { t.hooks.Store(h) }

type turnOnContextKey struct{}

// beginTurnContext returns ctx carrying the turn's scope (hook.WithScope,
// which also sets toolkit's agent id and session key) and hooks (which may be
// nil until use), and the turn's place on it. The host calls it once per
// turn, before anything of the turn reaches the context engine.
func beginTurnContext(ctx context.Context, scope hook.Scope, hooks *turnHooks, now clock.Clock) (context.Context, *turnOnContext) {
	ctx = hook.WithScope(ctx, scope)
	t := &turnOnContext{clock: now}
	t.hooks.Store(hooks)
	return context.WithValue(ctx, turnOnContextKey{}, t), t
}

// turnOf returns the turn ctx belongs to and its hooks; false outside a turn
// or before the turn has hooks.
func turnOf(ctx context.Context) (*turnOnContext, *turnHooks, bool) {
	t, ok := ctx.Value(turnOnContextKey{}).(*turnOnContext)
	if !ok || t == nil {
		return nil, nil, false
	}
	h := t.hooks.Load()
	return t, h, h != nil
}

// engineCompactionStart is the context engine's compaction-start hook
// (ctxengine.WithCompactionStartFunc): it reports the pass to the turn the
// context belongs to, as OnCompaction and a Compaction event (phase started),
// before the summary model is called. A pass with no turn on its context (a
// background pass) reaches no turn.
func engineCompactionStart(ctx context.Context, info ctxengine.CompactionStart) {
	t, hooks, ok := turnOf(ctx)
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
	hooks.callOnCompaction(ctx, ev)
	if t.live.Load() {
		hooks.emit(ctx, ev)
	}
}
