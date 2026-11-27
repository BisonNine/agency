package tools

import (
	"context"

	sessiontools "go.bison9.com/agent/ctxengine/tools"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

// ToolDescriptor holds static metadata for a single tool — no deps required.
type ToolDescriptor struct {
	Name           string // tool name as returned by Tool.Name()
	Description    string // one-line description for the GUI
	DefaultEnabled bool   // in the standard set the WebUI "Default tools" button writes
	// Suite, when non-empty, marks this as an all-or-nothing tool suite (e.g.
	// "cogmem", "maestro"): it is managed by a single per-agent toggle rather than
	// the per-tool allowlist, and the GUI renders it as one (read-only) entry
	// instead of listing every tool.
	Suite string
}

// SuiteProvider is an optional interface a tool provider implements to declare
// itself an all-or-nothing suite. Its tools are gated as a unit by the per-agent
// flag named by Suite() (resolved via Config.AgentSuiteEnabled), bypassing the
// per-tool allow/deny machinery, and are collapsed to a single GUI entry.
type SuiteProvider interface {
	Suite() string
}

// ToolProvider is implemented by each tool package. It declares what namespace
// it owns, whether it can run in the current environment, and how to build its
// tools given runtime dependencies.
type ToolProvider interface {
	Namespace() string // e.g. "files", "web", "session"
	Description() string
	Category() string
	ConfigKey() string                           // maps to config struct field name
	Available(cfg *config.Config) (bool, string) // (ok, reason if not)
	Build(deps ToolDeps) []toolkit.Tool
	Describe() []ToolDescriptor
}

// ToolDeps carries everything a tool package needs at construction time.
// Fields are optional — providers check for nil/zero before using.
type ToolDeps struct {
	Cfg       *config.Config
	AgentCfg  *config.AgentConfig // nil for the default agent
	AgentID   string
	Workspace string
	// StateDir holds the agent's conversation archive (sessions/) and its
	// cognitive memory (cogmem/): the workspace for a config agent, the
	// agent's own directory for a temporary one. Empty means Workspace.
	StateDir string
	// EphemeralMemory marks a memory nothing is consolidated into (a
	// sub-agent's snapshot of its source's memory).
	EphemeralMemory bool
	// TempAgent marks a temporary agent (a sub-agent clone). Its tools must
	// not replace anything registered once per agent id for the agent it acts
	// as (the Maestro runner): it uses what is there.
	TempAgent bool
	// TempPurpose is what a temporary agent was created for (TempPurposeForum
	// for a forum participant); empty for every other agent.
	TempPurpose string
	// CloneMarker is the short id of a clone (whose tools act as AgentID),
	// for the audit rows its calls write; "" for every other agent.
	CloneMarker string

	// Models is the agent's models by purpose and priority (its set in the
	// process's model catalogue).
	Models *models.Set

	// Spawn/subagent
	SpawnAllowlist func(callerID, targetID string) bool

	// Agents manages temporary agents on behalf of this agent (see
	// AgentServices).
	Agents AgentServices

	// Messenger sends this agent's agent_message asks and whispers.
	Messenger Messenger

	// Files sends files to a chat (msg_send_file).
	Files FileSender

	// Spawn holds the robust sub-agent launcher (a global.Spawner). The AgentLoop
	// builds it per agent and sets it here; toGlobalDeps forwards it to
	// toolspec.Deps.Spawn so any tool package can launch workers by DI. Typed as any
	// to keep tools free of an import dependency on the spawner implementation.
	Spawn any

	// Session tools (closures built by AgentLoop). CompactFn returns a
	// human-readable compaction report and the resulting rendered summary,
	// alongside any error.
	CompactFn     func(ctx context.Context, sessionKey string) (report, summary string, err error)
	SessionInfoFn func(ctx context.Context, sessionKey string) (*sessiontools.SessionInfo, error)
	// ClearFn clears the active conversation (preserving the archive) and hands
	// the agent a fresh turn, optionally delivering message as a self-handoff.
	// nil when session_clear is not enabled for the agent.
	ClearFn func(ctx context.Context, sessionKey, message string) error

	// Shared pre-built tool instances
	MessageTool toolkit.Tool // shared msg_send instance; may be nil

	// Billing returns the billing code of the work calling on ctx: the code
	// it inherited, else its session's, else the default. SetBilling changes
	// it (the billing tool): an inherited code for that work alone, else the
	// code of the session sessionKey; a code accounting.CheckProject refuses
	// is an error wrapping accounting.ErrInvalidProject. Both are nil outside
	// the agent loop; use BillingOf.
	Billing    func(ctx context.Context) string
	SetBilling func(ctx context.Context, sessionKey, code string) error
	// CallerBilling is Billing resolved from the call's own scope (its agent
	// and session) rather than this agent's, for a function kept beyond this
	// registration (Maestro's runner, which a clone shares with its source).
	// Nil outside the agent loop.
	CallerBilling func(ctx context.Context) string
	// Origin returns the origin of the work calling on ctx (accounting's
	// origin agent): the one it inherited, else this agent's own. Nil
	// outside the agent loop; use OriginOf.
	Origin func(ctx context.Context) string
}

// BillingOf is d.Billing(ctx), or, without one, the inherited code on ctx,
// else the default.
func (d ToolDeps) BillingOf(ctx context.Context) string {
	if d.Billing != nil {
		return d.Billing(ctx)
	}
	if code, ok := Billing(ctx); ok {
		return code
	}
	return global.DefaultBilling
}

// OriginOf is d.Origin(ctx), or, without one, the inherited origin on ctx.
func (d ToolDeps) OriginOf(ctx context.Context) string {
	if d.Origin != nil {
		return d.Origin(ctx)
	}
	return Origin(ctx)
}

// EffectiveStateDir is StateDir, or Workspace when StateDir is unset.
func (d ToolDeps) EffectiveStateDir() string {
	if d.StateDir != "" {
		return d.StateDir
	}
	return d.Workspace
}
