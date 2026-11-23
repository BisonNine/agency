// ClawEh
// License: MIT

package agent

import (
	"context"
	"fmt"

	sdkagent "go.bison9.com/agent"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/utils"
)

// This file builds each instance's SDK agent (go.bison9.com/agent/loop),
// the engine the host's turns are to run on. Turns do not run on it yet:
// they still run on the host's own engine (engine_*.go) until the SDK closes
// the gaps listed in refactor/refactor.md section 12. The agent is built
// with the instance and closed with it.

// newTurnAgent builds inst's SDK agent. It starts nothing.
func (al *AgentLoop) newTurnAgent(inst *AgentInstance) (sdkagent.Agent, error) {
	a, err := sdkagent.New(inst.ID, al.turnAgentSpec(inst))
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", inst.ID, err)
	}
	return a, nil
}

// turnAgentSpec is the host's settings of inst's SDK agent: its identity,
// tools, session store and session cache, model choice, prompt, limits and
// logging, each the host's own (hook/DESIGN.md section 10).
func (al *AgentLoop) turnAgentSpec(inst *AgentInstance) sdkagent.Option {
	return func(s *loop.Spec) {
		s.Name = inst.DisplayName()
		s.Identity = inst.Identity()
		if inst.Sessions != nil {
			s.Sessions = inst.Sessions
		}
		if inst.Tools != nil {
			s.Tools = agentTools{r: inst.Tools}
		}
		s.Temperature = inst.Temperature
		s.RepeatHint = fileEditRepeatHint
		s.Clock = registryClock{al: al}
		s.Logger = logger.SDK()
		s.Redactor = redactSecrets
		s.Truncator = utils.Truncate
		s.LogContent = logger.GetLogMessageContent

		s.TurnModels = func(ctx context.Context, scope hook.Scope) loop.TurnModels {
			tm := al.turnModelOf(ctx, inst, scope.Session)
			return loop.TurnModels{Chain: tm.chain, Limits: &modelspec.Limits{
				MaxTokens:     tm.limits.maxTokens,
				ContextWindow: tm.limits.contextWindow,
				NoTools:       tm.limits.noTools,
			}}
		}
		s.TurnLimits = func(context.Context, hook.Scope) hook.Limits {
			return hook.Limits{
				MaxIterations: inst.MaxIterations,
				ToolTimeout:   al.GetConfig().Agents.Defaults.GetToolTimeout(),
				TurnBudget:    hook.NoTurnBudget,
				Retries:       llmMaxRetries,
				RetryBackoff:  timeoutRetryBackoff,
			}
		}
		if inst.HumanModel != "" {
			// A human agent's turn is answered by the person: no session
			// context, prompt or tools.
			return
		}
		s.Session = func(ctx context.Context, scope hook.Scope) (loop.Session, error) {
			opts := turnOptsOf(ctx, scope)
			cm, mem, release := al.getSessionContext(inst, scope.Session) //nolint:contextcheck // compaction reporter: ctxengine's callback has no context
			sess := loop.Session{Context: cm, Release: release, Assemble: func(_ context.Context, req *ctxengine.AssembleRequest) {
				req.Channel, req.ChatID = opts.Channel, opts.ChatID
			}}
			if mem != nil {
				sess.Memory = cogmem.AsMemorySession(mem)
			}
			return sess, nil
		}
		s.InstructionLayers = func(ctx context.Context) []ctxengine.Layer {
			scope, _ := hook.ScopeOf(ctx)
			return al.promptLayers(inst, turnOptsOf(ctx, scope))
		}
		s.ToolContext = func(ctx context.Context, _ hook.ToolCall) context.Context {
			scope, _ := hook.ScopeOf(ctx)
			opts := turnOptsOf(ctx, scope)
			ctx = tools.WithToolAllowChecker(ctx, inst.Config)
			return tools.WithReplyChat(ctx, tools.ReplyChat{Channel: opts.Channel, ChatID: opts.ChatID})
		}
	}
}

// turnOptsKey carries a turn's options (as BeforeTurn leaves them) on the
// context given to the SDK agent's Run.
type turnOptsKey struct{}

// withTurnOpts returns ctx carrying opts for the SDK agent's callbacks.
func withTurnOpts(ctx context.Context, opts *processOptions) context.Context {
	return context.WithValue(ctx, turnOptsKey{}, opts)
}

// turnOptsOf is the turn's options on ctx; for a run given none, the
// options of a turn on the scope's session with no chat.
func turnOptsOf(ctx context.Context, scope hook.Scope) processOptions {
	if opts, ok := ctx.Value(turnOptsKey{}).(*processOptions); ok && opts != nil {
		return *opts
	}
	return processOptions{SessionKey: scope.Session}
}

// turnModelOf is the turn's model choice: the one the host made for the
// turn on ctx, else a new one.
func (al *AgentLoop) turnModelOf(ctx context.Context, inst *AgentInstance, sessionKey string) *turnModel {
	if opts, ok := ctx.Value(turnOptsKey{}).(*processOptions); ok && opts != nil && opts.turnModel != nil {
		return opts.turnModel
	}
	return al.selectTurnModel(inst, sessionKey)
}

// agentTools is an instance's tool registry as the SDK agent runs it: the
// catalogue's snapshot and version, and each call through the registry's
// own execution (its allow gate, the chat and the agent id on the call),
// for the chat of the turn on the call's context.
type agentTools struct{ r *tools.ToolRegistry }

var _ toolkit.Registry = agentTools{}

func (t agentTools) Snapshot() *toolkit.Snapshot { return t.r.Snapshot() }
func (t agentTools) Version() uint64             { return t.r.Version() }

func (t agentTools) Subscribe(fn func(toolkit.Change)) func() { return t.r.Subscribe(fn) }

func (t agentTools) Execute(ctx context.Context, name string, args map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	scope, _ := hook.ScopeOf(ctx)
	opts := turnOptsOf(ctx, scope)
	return t.r.ExecuteWithContext(ctx, name, args, opts.Channel, opts.ChatID, cb)
}
