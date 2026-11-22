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
	"maps"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// toolCallResult is one tool call of a batch and what it returned.
type toolCallResult struct {
	tc     spawnllm.ToolCall
	result *toolkit.Result
	// callbackPanic is a panic of a tool callback during the call, carried
	// to the turn's goroutine, where it ends the turn.
	callbackPanic any
}

// toolPanicError is the error of a tool call whose tool panicked. Its text is
// what it always was; a consumer tells a panic from other failures with
// errors.As.
type toolPanicError struct {
	tool  string
	value any
}

func (e *toolPanicError) Error() string { return fmt.Sprintf("tool %s panicked: %v", e.tool, e.value) }

// executeToolCalls runs a batch of tool calls in parallel and returns their
// results in call order. ToolCallStarted is emitted for every call, in call
// order, before the first call runs; each call's ToolCallFinished as it ends.
// A tool callback that panicked ends the turn here, once every call ended.
func (e *engineTurn) executeToolCalls(ctx context.Context, calls []spawnllm.ToolCall) []toolCallResult {
	results := make([]toolCallResult, len(calls))
	for i, tc := range calls {
		results[i].tc = tc
		e.hooks.emit(ctx, hook.ToolCallStarted{EventHeader: e.header(), Call: e.hookCall(tc)})
	}
	// Per-tool budget: a tool's context is cancelled at the deadline so a
	// well-behaved tool returns a timeout error and the model can continue the
	// turn. The turn's context is the backstop for a tool that ignores
	// cancellation.
	var toolTimeout time.Duration
	if e.spec.toolTimeout != nil {
		toolTimeout = e.spec.toolTimeout()
	}
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() { e.runToolCall(ctx, &results[i], toolTimeout) })
	}
	// Every call is waited for, however the turn's context ended, so each
	// has its result and its ToolCallFinished before the batch is stored.
	// Abandoning calls on an interrupt (hook.Limits.CancelGrace, then
	// hook.InterruptedResult) belongs here.
	wg.Wait()
	for _, r := range results {
		if r.callbackPanic != nil {
			panic(r.callbackPanic)
		}
	}
	return results
}

// runToolCall runs one tool call through the tool callbacks and stores its
// result in slot. A panicking tool becomes an error result, so it neither
// crashes the process nor wedges the batch; a panicking callback is carried
// to the turn (slot.callbackPanic). Every call ends with one
// ToolCallFinished.
func (e *engineTurn) runToolCall(ctx context.Context, slot *toolCallResult, toolTimeout time.Duration) {
	tc := slot.tc
	call := e.hookCall(tc)
	answered := false
	start := time.Now()
	execCtx, cancelExec := ctx, context.CancelFunc(func() {})
	if toolTimeout > 0 {
		execCtx, cancelExec = context.WithTimeout(ctx, toolTimeout)
	}
	defer func() {
		// The call's context is live until its ToolCallFinished is emitted.
		defer cancelExec()
		if r := recover(); r != nil {
			e.logAt(slog.LevelError, "panic in tool callback",
				map[string]any{
					"tool":  tc.Name,
					"panic": fmt.Sprintf("%v", r),
					"stack": string(debug.Stack()),
				})
			slot.callbackPanic = r
			if slot.result == nil {
				slot.result = toolkit.ErrorResult(fmt.Sprintf("Tool %s failed with an internal error.", tc.Name))
			}
		}
		ev := hook.ToolCallFinished{
			EventHeader: e.header(),
			Call:        hook.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: maps.Clone(tc.Arguments), Iteration: call.Iteration},
			Elapsed:     time.Since(start),
			Answered:    answered,
		}
		if slot.result != nil {
			snapshot := *slot.result
			ev.Result = &snapshot
		}
		e.hooks.emit(execCtx, ev)
	}()

	// Arguments are not logged: they routinely carry memory content, file
	// contents and other user data.
	e.logAt(slog.LevelInfo, "Tool call dispatched",
		e.turnFields(map[string]any{
			"agent_id":  e.spec.agentID,
			"tool":      tc.Name,
			"iteration": e.iteration,
		}))

	result := e.beforeTool(execCtx, &call)
	answered = result != nil
	if !answered {
		var panicked bool
		result, panicked = e.execute(execCtx, tc.Name, call.Arguments)
		if panicked {
			// A tool that panicked is not seen by the result callbacks.
			slot.result = result
			return
		}
	}
	// Set before the result callbacks, so a call whose callback panics still
	// finishes with the tool's result.
	slot.result = result
	slot.result = e.afterTool(execCtx, &call, result)
}

// execute runs the tool. A panicking tool is logged and becomes an error
// result (panicked set).
func (e *engineTurn) execute(ctx context.Context, name string, args map[string]any) (result *toolkit.Result, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			e.logAt(slog.LevelError, "panic in tool call",
				map[string]any{
					"tool":  name,
					"panic": fmt.Sprintf("%v", r),
					"stack": string(debug.Stack()),
				})
			result = &toolkit.Result{
				Err:    &toolPanicError{tool: name, value: r},
				ForLLM: fmt.Sprintf("Tool %s failed with an internal error.", name),
			}
			panicked = true
		}
	}()
	return e.spec.env.executeTool(ctx, name, args), false
}

// beforeTool runs the BeforeTool callbacks; a non-nil result answers the
// call instead of the tool, an error refuses it.
func (e *engineTurn) beforeTool(ctx context.Context, call *hook.ToolCall) *toolkit.Result {
	for _, fn := range e.hooks.beforeTool {
		result, err := fn(ctx, call)
		if err != nil {
			return toolkit.ErrorResult(err.Error()).WithError(err)
		}
		if result != nil {
			return result
		}
	}
	return nil
}

// afterTool runs OnToolError for an error result, then AfterTool.
func (e *engineTurn) afterTool(ctx context.Context, call *hook.ToolCall, result *toolkit.Result) *toolkit.Result {
	if result != nil && (result.IsError || result.Err != nil) {
		for _, fn := range e.hooks.onToolError {
			if r := fn(ctx, call, result); r != nil {
				result = r
			}
		}
	}
	for _, fn := range e.hooks.afterTool {
		fn(ctx, call, result)
	}
	return result
}

// toolImages routes the images a batch of tools returned according to the
// turn model's vision mode.
type toolImages struct {
	mode string
	// followUp is for VisionUserMessage: Chat Completions tool messages are
	// text-only, so the images go in one user message after the results.
	followUp []string
	// unseen and focus are for VisionOff: the images go to a describer
	// instead of being dropped, steered by the image tools' ForLLM text
	// (which carries any file_view_image focus line).
	unseen []string
	focus  []string
}

// route attaches result's images to its tool message or sets them aside.
func (ti *toolImages) route(result *toolkit.Result, msg *spawnllm.Message) {
	switch ti.mode {
	case modelspec.VisionToolResponse:
		// The OpenAI Responses API and xAI take images on the tool result itself.
		msg.Media = result.Images
	case modelspec.VisionUserMessage:
		ti.followUp = append(ti.followUp, result.Images...)
	default:
		if len(result.Images) > 0 {
			ti.unseen = append(ti.unseen, result.Images...)
			if f := strings.TrimSpace(result.ForLLM); f != "" {
				ti.focus = append(ti.focus, f)
			}
		}
	}
}

// addToolResults stores a batch's results in call order, one tool message per
// call, followed by any images the model is to see.
func (e *engineTurn) addToolResults(ctx context.Context, results []toolCallResult) error {
	images := toolImages{mode: e.spec.env.modelFacts(e.spec.model).vision}
	for _, r := range results {
		e.hooks.eachInterim(func(i interimHooks) {
			if i.ToolResult != nil {
				i.ToolResult(ctx, e.hookCall(r.tc), r.result)
			}
		})
		msg := e.toolResultMessage(r)
		images.route(r.result, &msg)
		if err := e.keep(ctx, e.spec.cm.AddToolResult, msg, "tool result"); err != nil {
			return err
		}
	}
	if err := e.addToolImages(ctx, images.followUp); err != nil {
		return err
	}
	return e.describeUnseenImages(ctx, images)
}

// toolResultMessage is the tool message for one result, its content capped
// to fit the context window.
func (e *engineTurn) toolResultMessage(r toolCallResult) spawnllm.Message {
	contentForLLM := r.result.ForLLM
	if contentForLLM == "" && r.result.Err != nil {
		contentForLLM = r.result.Err.Error()
	}
	if capped := capToolResult(contentForLLM, r.result.IsError, e.spec.contextWindow); len(capped) != len(contentForLLM) {
		e.logAt(slog.LevelWarn, "Tool result truncated for context", map[string]any{
			"agent_id":     e.spec.agentID,
			"tool":         r.tc.Name,
			"original_len": len(contentForLLM),
			"kept_len":     len(capped),
		})
		contentForLLM = capped
	}
	msg := spawnllm.Message{
		Role:       "tool",
		Content:    contentForLLM,
		ToolCallID: r.tc.ID,
		// The tools a discovery result found: spawnllm loads their deferred
		// definitions for every later request of this conversation.
		ToolReferences: r.result.ToolReferences,
	}
	// A failed write leaves the file unchanged, so the eviction sweep must
	// never treat it as superseding the read needed to correct it.
	if r.result.IsError {
		msg.Type = ctxengine.MessageTypeToolError
	}
	return msg
}

// addToolImages hands the batch's images to a VisionUserMessage model as one
// follow-up user message.
func (e *engineTurn) addToolImages(ctx context.Context, images []string) error {
	if len(images) == 0 {
		return nil
	}
	imgMsg := spawnllm.Message{
		Role:    "user",
		Content: "Image(s) returned by the tool call(s) above:",
		Media:   images,
	}
	if err := e.keep(ctx, e.spec.cm.AddUserMessage, imgMsg, "tool image message"); err != nil {
		return err
	}
	e.logAt(slog.LevelInfo, "passed tool image(s) to vision model (user message)",
		map[string]any{"agent_id": e.spec.agentID, "model": e.spec.model, "images": len(images)})
	return nil
}

// describeUnseenImages gives a text-only model a description of the images
// its tools returned, through the interim DescribeImages step, as a
// follow-up user message. Without a describer nothing is added: the model
// gets only the hidden-attachment note from messagesForModel.
func (e *engineTurn) describeUnseenImages(ctx context.Context, images toolImages) error {
	if images.mode != modelspec.VisionOff || len(images.unseen) == 0 {
		return nil
	}
	var content string
	var described, add bool
	e.hooks.eachInterim(func(i interimHooks) {
		if add || i.DescribeImages == nil {
			return
		}
		content, described, add = i.DescribeImages(ctx, images.unseen, images.focus)
	})
	if !add {
		return nil
	}
	if err := e.keep(ctx, e.spec.cm.AddUserMessage, spawnllm.Message{Role: "user", Content: content}, "vision description message"); err != nil {
		return err
	}
	e.logAt(slog.LevelInfo, "injected vision description for non-vision model",
		map[string]any{"agent_id": e.spec.agentID, "model": e.spec.model, "images": len(images.unseen), "described": described})
	return nil
}
