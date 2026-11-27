// Agency
// License: MIT

package tools

import (
	"context"
	"errors"
	"slices"
	"sync"
	"unicode/utf8"

	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
)

// Turn-scope values a turn hands to everything it starts: the tools it calls,
// the sub-agents it spawns, the agents it asks, and the turns its background
// results re-enter. They are carried on the context in process, on bus
// metadata between turns, and on the session token for a CLI agent's MCP
// calls.

type askChainKey struct{}

// WithAskChain returns ctx carrying chain: the agents waiting for a reply in
// the exchange the running turn belongs to, ending with the running agent.
func WithAskChain(ctx context.Context, chain []string) context.Context {
	return context.WithValue(ctx, askChainKey{}, chain)
}

// AskChain returns the ask chain on ctx (nil when none).
func AskChain(ctx context.Context) []string {
	if chain, ok := ctx.Value(askChainKey{}).([]string); ok {
		return chain
	}
	return nil
}

// WithAskChainAgent returns ctx with agentID appended to its ask chain (once).
func WithAskChainAgent(ctx context.Context, agentID string) context.Context {
	chain := AskChain(ctx)
	if agentID == "" || slices.Contains(chain, agentID) {
		return ctx
	}
	return WithAskChain(ctx, append(slices.Clone(chain), agentID))
}

type billingKey struct{}

// ErrNoBillingSession refuses a billing code change made where neither an
// inherited code nor a conversation is known (a call with a service token).
var ErrNoBillingSession = errors.New("no conversation to set the billing code for")

// InheritedBilling is the billing code of work that runs outside the session
// that owns a code: a sub-agent, a clone, a scheduled job's run, an asked
// turn, a Maestro worker, a forum participant, a turn replayed after a
// restart. The work may change it (the billing tool): the change applies to
// its own model calls from the next one and to whatever it starts afterwards,
// never to the session or work that started it, which holds a code of its
// own. Each run gets its own value (WithBilling); everything of one run (its
// turn, the tool calls it makes, over MCP too) shares it.
type InheritedBilling struct {
	mu   sync.Mutex
	code string
	// onSet are called, in order and outside the lock, with each code Set
	// stores, so the run's own record (its task record, its pending turn)
	// keeps it across a restart.
	onSet []func(code string)
}

// Code returns the code.
func (b *InheritedBilling) Code() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.code
}

// Set changes the code and tells the run's records (OnSet). The caller has
// checked code.
func (b *InheritedBilling) Set(code string) {
	b.mu.Lock()
	b.code = code
	fns := slices.Clone(b.onSet)
	b.mu.Unlock()
	for _, fn := range fns {
		fn(code)
	}
}

// OnSet adds fn to what Set calls with a new code.
func (b *InheritedBilling) OnSet(fn func(code string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onSet = append(b.onSet, fn)
}

// WithBilling returns ctx carrying a new inherited billing code for the work
// that runs on it, so nothing the work changes reaches the code ctx carried
// before. An empty code returns ctx without any inherited code.
func WithBilling(ctx context.Context, code string) context.Context {
	if code == "" {
		return context.WithValue(ctx, billingKey{}, (*InheritedBilling)(nil))
	}
	return context.WithValue(ctx, billingKey{}, &InheritedBilling{code: code})
}

// WithInheritedBilling returns ctx carrying b itself: the same run's code,
// for a context of that run built anew (an MCP tool call of its turn). A nil
// b removes the inherited code.
func WithInheritedBilling(ctx context.Context, b *InheritedBilling) context.Context {
	return context.WithValue(ctx, billingKey{}, b)
}

// InheritedBillingOf returns the inherited billing code on ctx, or nil when
// the work on ctx bills to its session's code.
func InheritedBillingOf(ctx context.Context) *InheritedBilling {
	if b, ok := ctx.Value(billingKey{}).(*InheritedBilling); ok {
		return b
	}
	return nil
}

// Billing returns the inherited billing code on ctx; ok is false when there
// is none.
func Billing(ctx context.Context) (code string, ok bool) {
	b := InheritedBillingOf(ctx)
	if b == nil {
		return "", false
	}
	return b.Code(), true
}

type originKey struct{}

// WithOrigin returns ctx naming origin as the origin of the work that runs
// on it: the id of the agent at the top of the chain of work (accounting's
// Identity.Origin). It is set where work is handed on, from the origin of
// the work handing it on (or the one kept in its record), and copied
// unchanged below; nothing changes it within the work, the billing tool
// included. An empty origin returns ctx naming none, so the work's origin is
// its own agent's; so does one longer than accounting allows (a hand-edited
// record), with a warning, so it never fails a model call.
func WithOrigin(ctx context.Context, origin string) context.Context {
	if utf8.RuneCountInString(origin) > accounting.MaxNameLength {
		logger.WarnCF("billing", "Invalid origin agent ignored; the working agent is used",
			map[string]any{"origin": origin})
		origin = ""
	}
	return context.WithValue(ctx, originKey{}, origin)
}

// Origin returns the origin inherited on ctx, "" when there is none (the
// work's origin is then its own agent's).
func Origin(ctx context.Context) string {
	if origin, ok := ctx.Value(originKey{}).(string); ok {
		return origin
	}
	return ""
}

// CheckedBilling is a billing code read back from where it was kept (a task
// record, a job, a classifier, a forum run, a message), ready to bill a call
// to: code when accounting.CheckProject accepts it; global.DefaultBilling
// for an empty one (written before billing codes) and, with one warning
// naming source, for one it refuses (a hand-edited file), so a bad code
// never fails a model call or reaches the accounting log.
func CheckedBilling(code, source string) string {
	if code == "" {
		return global.DefaultBilling
	}
	if err := accounting.CheckProject(code); err != nil {
		logger.WarnCF("billing", "Invalid billing code ignored; the default is used",
			map[string]any{"source": source, "code": code})
		return global.DefaultBilling
	}
	return code
}
