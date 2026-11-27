// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"

	sdkagent "go.bison9.com/agent"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/utils"
)

// This file builds each instance's SDK agent (go.bison9.com/agent/loop),
// the engine every turn runs on. The agent is built with the instance and
// closed with it; the host holds it as the agent.Agent interface. Each
// turn's own state rides on the context given to Run (withHostTurn), which
// every callback, tool and late result receives.

// newTurnAgent builds inst's SDK agent. It starts nothing.
func (al *AgentLoop) newTurnAgent(inst *AgentInstance) (sdkagent.Agent, error) {
	a, err := sdkagent.New(inst.ID, al.turnAgentSpec(inst))
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", inst.ID, err)
	}
	return a, nil
}

// turnAgentOf is inst's SDK agent. An instance the agent builder made has
// it already; one made otherwise (a test's) gets it on its first turn,
// unless the instance is closed: then the turn ends with loop.ErrClosed, as
// it does on a closed instance's agent, and nothing is built that no Close
// would close.
func (al *AgentLoop) turnAgentOf(inst *AgentInstance) (sdkagent.Agent, error) {
	inst.turnAgentMu.Lock()
	defer inst.turnAgentMu.Unlock()
	if inst.turnAgent == nil {
		if inst.closed {
			return nil, loop.ErrClosed
		}
		a, err := al.newTurnAgent(inst)
		if err != nil {
			return nil, err
		}
		inst.turnAgent = a
	}
	return inst.turnAgent, nil
}

// turnAgentSpec is the host's settings of inst's SDK agent: its identity,
// tools, session store and session cache, model choice, prompt, limits,
// logging and callbacks, each the host's own (hook/DESIGN.md section 10).
func (al *AgentLoop) turnAgentSpec(inst *AgentInstance) sdkagent.Option {
	return func(s *loop.Spec) {
		s.Name = inst.DisplayName()
		s.Identity = inst.Identity()
		// Each turn is billed to billingFor's code, asked again before every
		// model call, so a change (the billing tool) applies from the next
		// one; the agent's own code is the default, named.
		s.Identity.Project = global.DefaultBilling
		s.TurnProject = func(ctx context.Context, scope hook.Scope) string {
			return al.billingFor(ctx, inst, scope.Session)
		}
		// The turn's origin, asked as it is set up and by the compaction
		// after it: the one the turn inherited, else the agent's own.
		s.TurnOrigin = func(ctx context.Context, _ hook.Scope) string {
			return originFor(ctx, inst)
		}
		if inst.Sessions != nil {
			s.Sessions = inst.Sessions
		}
		if inst.Tools != nil {
			s.Tools = inst.Tools.Catalog()
			// The host's per-call grant check, on the exact tool the call
			// resolved to; a refusal names the tool as the model called it.
			s.Gate = func(ctx context.Context, info toolkit.Info) *toolkit.Result {
				call, _ := hook.ToolCallFrom(ctx)
				return tools.AllowGate(call.Name)(ctx, info)
			}
		}
		// One task registry for the process, across reloads and clones.
		s.Tasks = al.tasks
		s.Temperature = inst.Temperature
		s.RepeatHint = fileEditRepeatHint
		s.Clock = registryClock{al: al}
		s.Logger = logger.SDK()
		s.Redactor = redactSecrets
		s.Truncator = utils.Truncate
		s.LogContent = logger.GetLogMessageContent
		// The host serialises each session's turns itself (its session
		// queue); a path that bypasses it runs beside them, as it always has.
		s.Unqueued = true

		s.TurnModels = func(ctx context.Context, _ hook.Scope) loop.TurnModels {
			t := hostTurnOf(ctx)
			if t == nil || t.model == nil || t.model.chain == nil {
				return loop.TurnModels{}
			}
			return loop.TurnModels{Chain: t.model.chain, Limits: &modelspec.Limits{
				MaxTokens:     t.model.limits.maxTokens,
				ContextWindow: t.model.limits.contextWindow,
				NoTools:       t.model.limits.noTools,
			}}
		}
		s.TurnLimits = func(context.Context, hook.Scope) hook.Limits {
			iterations := inst.MaxIterations
			if iterations <= 0 {
				iterations = hook.NoIterations
			}
			return hook.Limits{
				MaxIterations: iterations,
				ToolTimeout:   al.GetConfig().Agents.Defaults.GetToolTimeout(),
				TurnBudget:    hook.NoTurnBudget,
				Retries:       hook.DefaultRetries,
				RetryBackoff:  hook.DefaultRetryBackoff,
			}
		}
		s.Plugins = al.turnPlugins(inst)
		if inst.HumanModel != "" {
			// A human agent's turn is answered by the person (BeforeTurn):
			// no session context, prompt or tools.
			s.Session = func(context.Context, hook.Scope) (loop.Session, error) {
				return loop.Session{}, nil
			}
			return
		}
		s.Session = func(ctx context.Context, scope hook.Scope) (loop.Session, error) {
			// The host holds the turn's reference on its cached session
			// context (getSessionContext) from before the run until after
			// it; the agent releases nothing of it.
			t := hostTurnOf(ctx)
			if t == nil || t.cm == nil {
				return loop.Session{}, errNoTurnSession
			}
			sess := loop.Session{
				Context: t.cm,
				Assemble: func(_ context.Context, req *ctxengine.AssembleRequest) {
					req.Channel, req.ChatID = t.opts.Channel, t.opts.ChatID
				},
				// The overflow retry compacts through a new reference on the
				// session's manager: after a reload during the turn, the
				// rebuilt one.
				Compaction: managerSource(sessionRef{al: al, inst: inst, key: scope.Session}).manager,
			}
			if t.mem != nil {
				sess.Memory = cogmem.AsMemorySession(t.mem)
			}
			return sess, nil
		}
		s.InstructionLayers = func(ctx context.Context) []ctxengine.Layer {
			t := hostTurnOf(ctx)
			if t == nil {
				return nil
			}
			return al.promptLayers(inst, *t.opts, t.model.protocol()) //nolint:contextcheck // the static prompt is cached across turns; listing the agent's knowledge bases is a local directory read no turn's end should cut short
		}
		// Every call carries who it is for (the turn's chat, none without a
		// host turn, and the agent id); a turn's call also its allow checker
		// and reply chat. The gate and the tool run on this context.
		s.ToolContext = func(ctx context.Context, _ hook.ToolCall) context.Context {
			t := hostTurnOf(ctx)
			if t == nil {
				return tools.CallContext(ctx, "", "")
			}
			ctx = tools.CallContext(ctx, t.opts.Channel, t.opts.ChatID)
			ctx = tools.WithToolAllowChecker(ctx, inst.Config)
			return tools.WithReplyChat(ctx, tools.ReplyChat{Channel: t.opts.Channel, ChatID: t.opts.ChatID})
		}
		// A background tool's late result goes to the chat and session the
		// call was made for, read from the context the call ran on.
		s.AsyncResult = func(ctx context.Context, call hook.ToolCall, _ toolkit.Info, r *toolkit.Result) {
			t := hostTurnOf(ctx)
			if t == nil {
				return
			}
			al.asyncToolCallback(ctx, inst, *t.opts, call.Name)(ctx, r)
		}
	}
}

// managerSource gives a new reference on a session's context manager.
type managerSource interface {
	manager() (ctxengine.ContextManager, func())
}

// sessionRef is one session of an instance, as a managerSource.
type sessionRef struct {
	al   *AgentLoop
	inst *AgentInstance
	key  string
}

func (r sessionRef) manager() (ctxengine.ContextManager, func()) {
	return r.al.getContextManager(r.inst, r.key)
}

// errNoTurnSession ends a run given no session context (withHostTurn).
var errNoTurnSession = errors.New("no session context for the turn")
