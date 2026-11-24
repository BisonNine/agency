// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package agent

import (
	"context"
	"errors"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// Billing codes. Every model call is recorded under a billing code (the
// accounting identity's Project). A session has one, stored in its state
// (CompactionState.Project) and kept until changed, "default" until then.
// Work that runs outside the session that owns a code (a sub-agent, a
// scheduled job's run, an asked turn, a Maestro worker, a forum participant,
// a replayed turn) inherits the code of the work that started it, carried on
// its context (tools.WithBilling). billingFor is the one rule.

// errNoBillingSession refuses a billing change made where no session and no
// inherited code is known.
var errNoBillingSession = errors.New("no conversation to set the billing code for")

// billingFor is the billing code of work for agent in sessionKey: the code
// the work inherited (on ctx), else the session's own, else DefaultBilling.
// An empty sessionKey means the session of the call on ctx (its scope, else
// its tool context); a nil agent has no session code.
func (al *AgentLoop) billingFor(ctx context.Context, agent *AgentInstance, sessionKey string) string {
	if code, ok := tools.Billing(ctx); ok {
		return code
	}
	if agent != nil {
		if sessionKey == "" {
			sessionKey = sessionKeyOf(ctx)
		}
		if code := al.sessionBillingCode(agent, sessionKey); code != "" {
			return code
		}
	}
	return global.DefaultBilling
}

// identityFor is agent's accounting identity for work in sessionKey, billed
// to billingFor's code.
func (al *AgentLoop) identityFor(ctx context.Context, agent *AgentInstance, sessionKey string) accounting.Identity {
	id := agent.Identity()
	id.Project = al.billingFor(ctx, agent, sessionKey)
	return id
}

// CallerBilling is the billing code of the work calling on ctx, for a caller
// that has only the context (scheduling a job, asking an agent, a Maestro
// task): the inherited code, else the session code of the agent and session
// in the call's scope, else DefaultBilling.
func (al *AgentLoop) CallerBilling(ctx context.Context) string {
	if code, ok := tools.Billing(ctx); ok {
		return code
	}
	scope, _ := hook.ScopeOf(ctx)
	if scope.AgentID != "" {
		if reg := al.GetRegistry(); reg != nil {
			if inst, ok := reg.Get(scope.AgentID); ok && inst != nil {
				return al.billingFor(ctx, inst, scope.Session)
			}
		}
	}
	return global.DefaultBilling
}

// sessionKeyOf is the session of the call on ctx: its scope's, else the one
// its tool context names.
func sessionKeyOf(ctx context.Context) string {
	if scope, ok := hook.ScopeOf(ctx); ok && scope.Session != "" {
		return scope.Session
	}
	return toolkit.SessionKey(ctx)
}

// sessionBillingCode is the billing code stored for the session, "" when
// none is (or it cannot be read), loading it on a cache miss.
func (al *AgentLoop) sessionBillingCode(agent *AgentInstance, sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	key := activeModelCacheKey(agent.ID, sessionKey)
	if v, ok := al.sessionBilling.Load(key); ok {
		if code, isString := v.(string); isString {
			return code
		}
	}
	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		return ""
	}
	st, err := store.GetCompactionState(sessionKey)
	if err != nil {
		// Unreadable now: read again next time rather than caching "none".
		return ""
	}
	code := st.Project
	if code != "" && accounting.CheckProject(code) != nil {
		logger.WarnCF("agent", "Ignoring invalid stored billing code",
			map[string]any{"agent_id": agent.ID, "session_key": sessionKey})
		code = ""
	}
	al.sessionBilling.Store(key, code)
	return code
}

// forgetSessionBilling drops the cached billing code of agentID's session,
// whose state row is being deleted: a later conversation under the key
// starts with no code.
func (al *AgentLoop) forgetSessionBilling(agentID, sessionKey string) {
	al.sessionBilling.Delete(activeModelCacheKey(agentID, sessionKey))
}

// setSessionBilling stores code as the session's billing code. The caller
// has checked it.
func (al *AgentLoop) setSessionBilling(agent *AgentInstance, sessionKey, code string) error {
	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		return errNoBillingSession
	}
	st, err := store.GetCompactionState(sessionKey)
	if err != nil {
		logger.WarnCF("agent", "billing code: load compaction state failed",
			map[string]any{"session_key": sessionKey, "error": err.Error()})
		return err
	}
	st.Project = code
	if err := store.SetCompactionState(sessionKey, st); err != nil {
		logger.WarnCF("agent", "billing code: persist failed",
			map[string]any{"session_key": sessionKey, "error": err.Error()})
		al.alertSessionStateNotPersisted(agent, "billing code", err)
		return err
	}
	al.storeFaultOver(agent, alertSessionStateNotStored)
	al.sessionBilling.Store(activeModelCacheKey(agent.ID, sessionKey), code)
	return nil
}

// setBilling changes the billing code of the work calling on ctx for agent
// in sessionKey (the billing tool): an inherited code is changed for that
// work alone, else the session's own code. It returns an error wrapping
// accounting.ErrInvalidProject for a code CheckProject refuses.
func (al *AgentLoop) setBilling(ctx context.Context, agent *AgentInstance, sessionKey, code string) error {
	if err := accounting.CheckProject(code); err != nil {
		return err
	}
	if b := tools.InheritedBillingOf(ctx); b != nil {
		b.Set(code)
		return nil
	}
	if sessionKey == "" {
		sessionKey = sessionKeyOf(ctx)
	}
	if sessionKey == "" {
		return errNoBillingSession
	}
	return al.setSessionBilling(agent, sessionKey, code)
}

// withInboundBilling puts the billing code an inbound message carries
// (bus.MetaBilling) on ctx as the turn's inherited code, checked
// (tools.CheckedBilling: one that is not a code is the default).
func withInboundBilling(ctx context.Context, msg bus.InboundMessage) context.Context {
	code := inboundMetadata(msg, bus.MetaBilling)
	if code == "" {
		return ctx
	}
	return tools.WithBilling(ctx, tools.CheckedBilling(code, "message on "+msg.Channel))
}

// inheritedBilling is the code of the turn on ctx when it was inherited,
// "" when the turn bills to its session's code: what a message the turn
// sends on (an ask, a background result) carries.
func inheritedBilling(ctx context.Context) string {
	code, _ := tools.Billing(ctx)
	return code
}
