// Agency
// License: MIT

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"sync"
	"time"
	"weak"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/mcphost"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/agenttoken"
	"go.bison9.com/agency/app"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// invalidTokenMessage is what we return when the supplied session_token is
// missing, malformed, or unknown. The wording is intentionally instructive
// so a confused LLM can self-correct on the next call.
const invalidTokenMessage = "invalid or missing session_token; supply your assigned token (format: SST<64 hex>)"

// subagentMessage is returned when the literal sub-agent sentinel is used.
var subagentMessage = "sub-agents are not granted " + app.ShortName() + " MCP access; use the harness filesystem tools against your assigned working directory."

// tokenDescription describes the session_token argument of every tool
// published on /internal.
var tokenDescription = "Your session_token (format: 'SST<64 hex>'). Required on every " + global.HostMCPToolPrefix + "* call. Supplied verbatim from the agent's system prompt."

// firstCallTracker debounces "first MCP call from agent=<name>" log entries
// so we emit at most one per agent per server lifetime.
type firstCallTracker struct {
	mu    sync.Mutex
	seen  map[string]bool
	known map[string]string // agentName -> workspace (for the boot-log message)
}

func newFirstCallTracker(workspaces map[string]string) *firstCallTracker {
	known := make(map[string]string, len(workspaces))
	maps.Copy(known, workspaces)
	return &firstCallTracker{
		seen:  make(map[string]bool),
		known: known,
	}
}

func (t *firstCallTracker) record(agentName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen[agentName] {
		return
	}
	t.seen[agentName] = true
	logger.InfoCF("mcpserver", "MCP call from agent",
		map[string]any{
			"agent":     agentName,
			"workspace": t.known[agentName],
		})
}

// workspace returns the recorded workspace for agentName, or "" if none was
// supplied at construction. Safe for concurrent use; the known map is
// effectively read-only after newFirstCallTracker.
func (t *firstCallTracker) workspace(agentName string) string {
	if t == nil {
		return ""
	}
	return t.known[agentName]
}

// refusal is a credential an /internal call is refused for, with the log
// line that says why. The refusal is the call's answer (BeforeTool), so it
// is logged with the tool it was presented for.
type refusal struct {
	text, log, reason string
	fields            map[string]any
}

// callerValue is the host's data about an authenticated caller
// (mcphost.Caller.Value).
type callerValue struct {
	// refused, when set, is why the credential was refused: the call is
	// answered with it and nothing else runs.
	refused *refusal
	rec     sessionRecord
	// reg is the agent's registry; nil when the agent has none, and every
	// call is refused as such.
	reg *tools.ToolRegistry
}

// callerValueOf returns the host's data about the caller of ctx's call.
func callerValueOf(ctx context.Context) (callerValue, bool) {
	c, ok := mcphost.CallerOf(ctx)
	if !ok {
		return callerValue{}, false
	}
	v, ok := c.Value.(callerValue)
	return v, ok
}

// destKey carries a call's reply chat (CallContext); resolvedKey the tool
// the call resolved to and the ACL's decision on it (ToolContext).
type (
	destKey     struct{}
	resolvedKey struct{}
)

// resolved is the tool a call resolved to and whether the ACL allows the
// caller's agent to call it, decided once per call.
type resolved struct {
	name    string
	allowed bool
}

// callDest returns the reply chat CallContext put on ctx.
func callDest(ctx context.Context) (tools.ReplyChat, bool) {
	d, ok := ctx.Value(destKey{}).(tools.ReplyChat)
	return d, ok
}

// resolvedTool returns the tool the call on ctx resolved to; its name is ""
// when the caller's set does not hold it.
func resolvedTool(ctx context.Context) resolved {
	r, ok := ctx.Value(resolvedKey{}).(resolved)
	if !ok {
		return resolved{}
	}
	return r
}

// emptySet serves no tools: the set of a refused caller and of an agent with
// no registry.
var emptySet = toolkit.New().Default()

// endpointHost is the host's side of one endpoint: who a credential is, the
// views of each agent's registry the endpoint serves, the per-call context,
// the gate and the callbacks. The two endpoints differ only in transport and
// visibility filter.
type endpointHost struct {
	m      *MCPServer
	bearer bool
	allow  []string // the endpoint's visibility filter (internal names)
	// views holds each agent's view, built on first use. An entry is keyed
	// by the agent and its registry and dropped once the registry is gone (a
	// temporary agent deleted), so it never keeps one alive.
	viewsMu sync.Mutex
	views   map[viewKey]*toolkit.Set
}

// viewKey names one agent's view of one registry.
type viewKey struct {
	agent string
	reg   weak.Pointer[tools.ToolRegistry]
}

// visibility grants the tools an endpoint's filter admits, by internal name.
type visibility []string

func (v visibility) IsToolAllowed(name string) bool { return config.MatchVisibility(v, name) }

// callerGrant is what one agent is served on an endpoint: the endpoint's
// filter, and on /mcp the ACL too (its tools/list offers only what the agent
// may call).
type callerGrant struct {
	allow  visibility
	acl    acl.Policy
	agent  string
	bearer bool
}

func (g callerGrant) IsToolAllowed(name string) bool {
	if !g.allow.IsToolAllowed(name) {
		return false
	}
	return !g.bearer || g.acl.IsAllowed(g.agent, name)
}

func newEndpointHost(m *MCPServer, bearer bool, allow []string) *endpointHost {
	return &endpointHost{m: m, bearer: bearer, allow: allow, views: map[viewKey]*toolkit.Set{}}
}

// view returns agentID's view of reg on this endpoint.
func (h *endpointHost) view(agentID string, reg *tools.ToolRegistry) *toolkit.Set {
	key := viewKey{agent: agentID, reg: weak.Make(reg)}
	h.viewsMu.Lock()
	defer h.viewsMu.Unlock()
	if set, ok := h.views[key]; ok {
		return set
	}
	set := reg.View(callerGrant{allow: h.allow, acl: h.m.policy, agent: agentID, bearer: h.bearer})
	h.views[key] = set
	runtime.AddCleanup(reg, func(k viewKey) {
		h.viewsMu.Lock()
		delete(h.views, k)
		h.viewsMu.Unlock()
	}, key)
	return set
}

// catalogue is the endpoint's published tools: the union of the config
// agents' registries under the endpoint's filter, each tool described by the
// first agent (by id) that has it.
func (h *endpointHost) catalogue() mcphost.Catalogue {
	ids := sortedKeys(h.m.agentRegistries)
	filtered := make([]toolkit.Registry, 0, len(ids))
	for _, id := range ids {
		if reg := h.m.agentRegistries[id]; reg != nil {
			filtered = append(filtered, reg.View(visibility(h.allow)))
		}
	}
	return mcphost.Union(func() []toolkit.Registry { return filtered })
}

// authenticate resolves a token to the agent and session it names. On /mcp
// a refused token is a 401, as the bearer check before dispatch gave it. On
// /internal every token is accepted here and a refused one answers the call
// with today's text (refusal), so the refusal is logged with its tool.
func (h *endpointHost) authenticate(_ context.Context, token string) (mcphost.Caller, error) {
	st := h.m.sessionTokens
	refuse := func(r refusal) (mcphost.Caller, error) {
		if h.bearer {
			return mcphost.Caller{}, fmt.Errorf("%s", r.reason)
		}
		return mcphost.Caller{Set: emptySet, Value: callerValue{refused: &r}}, nil
	}
	switch {
	case agenttoken.IsSubagentSentinel(token):
		return refuse(refusal{text: subagentMessage, log: "MCP token rejected: subagent sentinel", reason: "subagent_sentinel"})
	case token == "":
		return refuse(refusal{text: invalidTokenMessage, log: "MCP tool call presented no session_token", reason: "no_token"})
	case st == nil:
		return refuse(refusal{text: invalidTokenMessage, log: "MCP token rejected: token store unavailable", reason: "no_token_store"})
	}
	rec, found := st.Resolve(token)
	if !found {
		return refuse(refusal{
			text: invalidTokenMessage, log: "MCP session token verification failed", reason: "invalid_token",
			fields: map[string]any{"token_len": len(token)},
		})
	}
	scope := hook.Scope{AgentID: rec.agentID, Session: rec.sessionKey}
	reg, ok := h.m.registryFor(rec.agentID)
	if !ok || reg == nil {
		return mcphost.Caller{Set: emptySet, Scope: scope, Value: callerValue{rec: rec}}, nil
	}
	set := h.view(rec.agentID, reg)
	// Done stays nil: a call in flight finishes whatever happens to its
	// token (a reissue on interrupt, a session clear).
	return mcphost.Caller{Set: set, Scope: scope, Value: callerValue{rec: rec, reg: reg}}, nil
}

// callContext puts the turn scope of the caller's token on the call: its
// sub-agent depth, ask chain and task, and the chat of the turn the call
// belongs to, as the tool context and the reply chat (see BeginTurn).
func (h *endpointHost) callContext(ctx context.Context, c mcphost.Caller) context.Context {
	v, ok := c.Value.(callerValue)
	if !ok || v.refused != nil {
		return ctx
	}
	rec := v.rec
	// Run at the sub-agent depth of the turn that holds this token, as the
	// in-process loop does: a CLI provider's tool calls (agent_spawn, Maestro
	// dispatch) arrive here, not through the loop, and would otherwise start at
	// depth 0 whatever the turn's depth.
	ctx = toolsagents.WithSpawnDepth(ctx, rec.depth)
	// Likewise the turn's ask chain, the task it runs in and its inherited
	// billing code and origin (SetTurnScope), with the agent's view of the
	// task registry.
	ctx = tools.WithAskChain(ctx, rec.askChain)
	ctx = tools.WithInheritedBilling(ctx, rec.billing)
	var origin string
	if rec.origin != nil {
		origin = *rec.origin
	}
	ctx = tools.WithOrigin(ctx, origin)
	scope, _ := hook.ScopeOf(ctx)
	scope.Task, scope.Depth, scope.Origin = rec.taskID, rec.depth, origin
	ctx = hook.WithScope(ctx, scope)
	if h.m.taskRegistrar != nil {
		ctx = hook.WithTaskRegistrar(ctx, h.m.taskRegistrar(rec.agentID))
	}

	// Everything the call sends to "the current chat" (msg_send,
	// msg_send_file, ForUser, breadcrumbs, background results, a
	// session_clear restart) goes to the chat of the turn the call belongs
	// to, as in the agent loop: kept per turn, so a concurrent turn of the
	// session never lends its chat. A service token, a session with no turn
	// running and one with several have no chat (dest's Target is empty).
	dest, inTurn := h.m.sessionTokens.ReplyChat(rec)
	if inTurn {
		ctx = tools.WithReplyChat(ctx, dest)
	}
	ctx = context.WithValue(ctx, destKey{}, dest)
	destChannel, destChatID := dest.Target()
	return tools.CallContext(ctx, destChannel, destChatID)
}

// toolContext gives the session key only to session-scoped tools, which
// read it with toolkit.SessionKey, and records the tool the call resolved
// to, with the ACL's decision on it, for the callbacks and the gate.
func (h *endpointHost) toolContext(ctx context.Context, c mcphost.Caller, info toolkit.Info) context.Context {
	ctx = context.WithValue(ctx, resolvedKey{}, resolved{name: info.Name, allowed: h.m.policy.IsAllowed(c.Scope.AgentID, info.Name)})
	if t, ok := info.Tool.(toolkit.SessionScoped); ok && t.IsSessionScoped() {
		return ctx
	}
	return toolkit.WithSessionKey(ctx, "")
}

// gate runs on the exact tool a call resolved to: the ACL (decided when the
// call resolved), then the registry's own grant check.
func (h *endpointHost) gate(ctx context.Context, info toolkit.Info) *toolkit.Result {
	// The endpoint runs the tool the call resolved to; any other is refused.
	if r := resolvedTool(ctx); r.name != info.Name || !r.allowed {
		return toolkit.ErrorResult(toolkit.NotEnabledMessage(info.Name)).
			WithError(toolkit.Refusal(fmt.Errorf("tool not permitted: %s", info.Name)))
	}
	return tools.CallGate(ctx, info)
}

// beforeTool is the host's per-call checks and notices, in today's order:
// a refused credential, the agent's registry, the tool in it, the ACL; then
// the authorized line, the first-call line and the "/tools on" breadcrumb.
func (h *endpointHost) beforeTool(ctx context.Context, call *hook.ToolCall) (*toolkit.Result, error) {
	v, ok := callerValueOf(ctx)
	if !ok {
		return nil, nil
	}
	// The internal name of the published tool, as the log lines name it.
	toolName := internalName(ctx, call.Name)
	if r := v.refused; r != nil {
		fields := map[string]any{"tool": toolName, "reason": r.reason}
		maps.Copy(fields, r.fields)
		logger.WarnCF("mcpserver", r.log, fields)
		return toolkit.ErrorResult(r.text), nil
	}
	rec := v.rec
	agentName := rec.agentID
	logger.InfoCF("mcpserver", "MCP session token verified",
		map[string]any{"agent": agentName, "session": rec.sessionKey, "tool": toolName})
	if v.reg == nil {
		logger.WarnCF("mcpserver", "MCP token rejected: no registry for agent",
			map[string]any{"tool": toolName, "agent": agentName, "reason": "no_registry"})
		return toolkit.ErrorResult(fmt.Sprintf("agent %q has no registered tool registry", agentName)), nil
	}
	r := resolvedTool(ctx)
	internal := r.name
	if internal == "" {
		// Not in the agent's tools on this endpoint; the endpoint refuses it as
		// not enabled, by its internal name. On /mcp that includes a tool the
		// ACL denies.
		if _, has := v.reg.Get(toolName); has && !h.m.policy.IsAllowed(agentName, toolName) {
			logger.WarnCF("mcpserver", "MCP tool denied",
				map[string]any{"agent": agentName, "tool": toolName, "reason": "acl_denied"})
		} else {
			logger.WarnCF("mcpserver", "MCP tool not in agent registry",
				map[string]any{"agent": agentName, "tool": toolName, "reason": "tool_not_in_registry"})
		}
		return nil, nil
	}
	if !r.allowed {
		logger.WarnCF("mcpserver", "MCP tool denied",
			map[string]any{"agent": agentName, "tool": internal, "reason": "acl_denied"})
		return toolkit.ErrorResult(toolkit.NotEnabledMessage(internal)).
			WithError(toolkit.Refusal(fmt.Errorf("tool not permitted: %s", internal))), nil
	}
	logger.InfoCF("mcpserver", "MCP tool authorized",
		map[string]any{"agent": agentName, "tool": internal, "workspace": h.m.tracker.workspace(agentName)})
	h.m.tracker.record(agentName)

	// Follow-along breadcrumb (/tools on): a one-line, privacy-safe note published
	// to the originating user before the tool runs — parity with the agent loop, so
	// tool calls made by CLI providers over MCP are no longer silent.
	if h.m.toolActivity != nil {
		if line := h.m.toolActivity(agentName, rec.sessionKey, internal, call.Arguments); line != "" {
			dest, _ := callDest(ctx)
			publishMCPToolActivity(ctx, h.m.msgBus, rec, dest, internal, line)
		}
	}
	return nil, nil
}

// afterTool redacts any token from what the model receives and sends a
// tool's own output to the chat of the call's turn while tool output is on
// (stream_tool_activity). The call is counted when it ends (onEvent).
func (h *endpointHost) afterTool(ctx context.Context, call *hook.ToolCall, res *toolkit.Result) {
	res.ForLLM = agenttoken.Redact(res.ForLLM)
	v, ok := callerValueOf(ctx)
	if !ok || v.refused != nil || panicked(res.Err) {
		return
	}
	name := internalName(ctx, call.Name)
	// The model's answer carries only ForLLM; ForUser is delivered out of band
	// over the message bus to the chat of the call's turn, only when tool
	// output is on, as in the agent loop. Background results: asyncResult.
	if h.m.toolOutput != nil && h.m.toolOutput() {
		dest, _ := callDest(ctx)
		publishMCPForUser(ctx, h.m.msgBus, v.rec, dest, name, res)
	}
}

// onEvent counts each call in the session's tool statistics when it ends,
// as AfterTool left its result. A call whose tool, Gate or host callback
// panicked counts as a failure under the panic's own text, as a panicking
// tool always has; its answer is MCP's internal error. A callback's panic
// skips the call's later callbacks, so only the end of the call sees every
// panic, and each call ends exactly once.
func (h *endpointHost) onEvent(ctx context.Context, ev hook.Event) {
	fin, ok := ev.(hook.ToolCallFinished)
	if !ok {
		return
	}
	v, ok := callerValueOf(ctx)
	if !ok || v.refused != nil {
		return
	}
	name := internalName(ctx, fin.Call.Name)
	res := fin.Result
	if panicked(res.Err) {
		res = toolkit.ErrorResult("tool " + name + " panicked")
	}
	tools.RecordToolResult(v.rec.sessionKey, name, &toolkit.Result{ForLLM: res.ForLLM, IsError: res.IsError})
}

// panicked reports that err is a panic the endpoint recovered: the tool's
// or the Gate's (*hook.ToolPanicError) or a host callback's
// (*hook.PanicError).
func panicked(err error) bool {
	var tp *hook.ToolPanicError
	var cp *hook.PanicError
	return errors.As(err, &tp) || errors.As(err, &cp)
}

// asyncResult delivers a background tool's late result the way the agent
// loop does: ForUser to the user, and ForLLM re-injected as a "system"
// message into the agent's session, at the dispatching call's spawn depth.
// ctx is the context the call ran on (detached from the request), so the
// result answers the chat of the call's turn, captured when the call was
// made, whatever turns run now.
func (h *endpointHost) asyncResult(ctx context.Context, c mcphost.Caller, _ hook.ToolCall, info toolkit.Info, r *toolkit.Result) {
	v, ok := c.Value.(callerValue)
	if !ok || v.refused != nil {
		return
	}
	dest, _ := callDest(ctx)
	publishMCPForUser(ctx, h.m.msgBus, v.rec, dest, info.Name, r)
	publishMCPAsyncToLLM(ctx, h.m.msgBus, v.rec, dest, info.Name, r, v.rec.depth)
}

// inheritedCode is the inherited billing code of the call on ctx, "" when
// its turn bills to its session's code.
func inheritedCode(ctx context.Context) string {
	code, _ := tools.Billing(ctx)
	return code
}

// internalName is the internal name of the tool the call on ctx was
// published for (mcphost.TargetOf), or name when there is none.
func internalName(ctx context.Context, name string) string {
	if t, ok := mcphost.TargetOf(ctx); ok && t.Internal != "" {
		return t.Internal
	}
	return name
}

// config is the endpoint's mcphost configuration.
func (h *endpointHost) config() mcphost.Config {
	cfg := mcphost.Config{
		// Protocol handshake: bare semver, not app.Version(). A client may
		// compare serverInfo.version, so it must not carry a build suffix.
		Name: app.Name(), Version: app.SemVer(),
		Transport:              mcphost.Argument,
		TokenDescription:       tokenDescription,
		Authenticator:          mcphost.AuthenticatorFunc(h.authenticate),
		Catalogue:              h.catalogue(),
		Names:                  mcphost.ExternalFirst,
		RefuseWithInternalName: true,
		PanicAsProtocolError:   true,
		OnSkip:                 logSkip,
		OnChange:               logChange,
		Gate:                   h.gate,
		CallContext:            h.callContext,
		ToolContext:            h.toolContext,
		Plugins: []hook.Plugin{{
			Name:       "mcpserver",
			BeforeTool: h.beforeTool,
			AfterTool:  h.afterTool,
			OnEvent:    h.onEvent,
		}},
		AsyncResult: h.asyncResult,
	}
	if h.bearer {
		cfg.Transport = mcphost.Bearer
		cfg.TokenDescription = ""
		cfg.Realm = global.HostMCPRealm
	}
	return cfg
}

// logChange logs, at debug level, the tools an endpoint registered and the
// names it withdrew, as the host's catalogue build always has (each
// endpoint, at start and on every refresh that changed what it publishes).
func logChange(r mcphost.RefreshResult) {
	for _, t := range r.Added {
		logger.DebugCF("mcpserver", "registered tool", map[string]any{"tool": t.Internal})
	}
	if len(r.Removed) > 0 {
		names := make([]string, len(r.Removed))
		for i, t := range r.Removed {
			names[i] = t.Name
		}
		logger.DebugCF("mcpserver", "removed tools", map[string]any{"tools": names})
	}
}

// logSkip logs a tool an endpoint leaves out of what it publishes, as the
// host's catalogue build always has (each endpoint, at start and on every
// refresh).
func logSkip(s mcphost.Skip) {
	switch s.Reason {
	case mcphost.SkipNameTaken:
		logger.WarnCF("mcpserver", "skipping tool: external name collision",
			map[string]any{"external": s.Published, "tool": s.Internal, "conflicts_with": s.Other})
	case mcphost.SkipSchema:
		// The encoder's own error, as the host's build logged it.
		errText := ""
		if err := errors.Unwrap(s.Err); err != nil {
			errText = err.Error()
		} else if s.Err != nil {
			errText = s.Err.Error()
		}
		logger.WarnCF("mcpserver", "skipping tool: failed to marshal schema",
			map[string]any{"tool": s.Internal, "error": errText})
	case mcphost.SkipRefused:
		// The host's own build registered every encodable schema, and MCP's
		// refusal of one stopped it; the endpoint now leaves that tool out.
		logger.WarnCF("mcpserver", "skipping tool: MCP refused its schema",
			map[string]any{"tool": s.Internal, "external": s.Published})
	}
}

// publishMCPAsyncToLLM re-injects an async tool's completion (ForLLM, or the
// error) into the agent's session as a "system" message, so the primary LLM is
// notified of the result without polling. Mirrors the agent-loop async path
// (agent/tool_exec.go): the result answers the chat of the call's turn, dest.
// No-op when there is nothing to inject; when the call had no turn chat to
// route to, it logs the drop rather than failing silently.
func publishMCPAsyncToLLM(ctx context.Context, msgBus *bus.MessageBus, rec sessionRecord, dest tools.ReplyChat, toolName string, r *toolkit.Result, spawnDepth int) {
	if r == nil || msgBus == nil {
		return
	}
	content := r.ForLLM
	if content == "" && r.Err != nil {
		content = r.Err.Error()
	}
	if content == "" {
		return
	}
	channel, chatID := dest.Target()
	if channel == "" || chatID == "" {
		// The call had no turn chat (a service token, no turn, several turns),
		// so the re-injected completion cannot be routed. Log it rather than
		// dropping silently.
		logger.WarnCF("mcpserver", "mcp.async.reinject_dropped",
			map[string]any{
				"tool":        toolName,
				"agent":       rec.agentID,
				"session_key": rec.sessionKey,
				"reason":      noChatReason(dest),
			})
		return
	}
	// A temporary clone's late result goes to its source's main conversation
	// (resolved when its token was issued): the clone may be gone by now.
	targetAgent, targetSession := rec.agentID, rec.sessionKey
	if rec.homeID != "" {
		targetAgent, targetSession = rec.homeID, routing.BuildAgentMainSessionKey(rec.homeID)
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := msgBus.PublishInbound(pubCtx, bus.InboundMessage{
		Channel:    "system",
		SenderID:   "async:" + toolName,
		Internal:   true,
		ChatID:     chatID,
		Content:    content,
		SessionKey: targetSession,
		// The re-entered turn keeps the call's turn's inherited code and
		// origin.
		Metadata: bus.SetOrigin(bus.SetBilling(bus.SetSpawnDepth(map[string]string{
			"preresolved_agent_id": targetAgent, bus.MetaOriginChannel: channel,
		}, spawnDepth), inheritedCode(ctx)), tools.Origin(ctx)),
	}); err != nil {
		logger.WarnCF("mcpserver", "mcp.async.reinject_failed",
			map[string]any{"tool": toolName, "agent": rec.agentID, "error": err.Error()})
		return
	}
	logger.InfoCF("mcpserver", "mcp.async.reinject",
		map[string]any{
			"tool":        toolName,
			"agent":       rec.agentID,
			"session_key": rec.sessionKey,
			"channel":     channel,
			"content_len": len(content),
		})
}

// publishMCPToolActivity sends a "/tools on" breadcrumb line to the chat of
// the call's turn, dest, over the outbound bus. No-op when the bus is absent or
// the call has no chat (best-effort, like the ForUser side channel).
func publishMCPToolActivity(ctx context.Context, msgBus *bus.MessageBus, rec sessionRecord, dest tools.ReplyChat, toolName, line string) {
	// An internal turn (an asked one's) has no chat to show it in.
	channel, chatID := dest.Target()
	if msgBus == nil || channel == "" || chatID == "" || line == "" || constants.IsInternalChannel(channel) {
		return
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Content: line,
	}); err != nil {
		logger.WarnCF("mcpserver", "mcp.toolactivity.publish_failed",
			map[string]any{"tool": toolName, "agent": rec.agentID, "error": err.Error()})
	}
}

// publishMCPForUser sends a tool's ForUser payload to the chat of the call's
// turn, dest, via the outbound message bus, when a payload exists, the result
// is not Silent, and the call has a chat. Logged drop in every other case —
// the MCP caller's ForLLM response is unaffected.
func publishMCPForUser(
	ctx context.Context,
	msgBus *bus.MessageBus,
	rec sessionRecord,
	dest tools.ReplyChat,
	toolName string,
	result *toolkit.Result,
) {
	if result == nil || result.Silent || result.ForUser == "" {
		return
	}
	if msgBus == nil {
		logger.WarnCF("mcpserver", "mcp.foruser.dropped",
			map[string]any{
				"tool":        toolName,
				"agent":       rec.agentID,
				"session_key": rec.sessionKey,
				"reason":      "no_bus",
			})
		return
	}
	channel, chatID := dest.Target()
	if channel == "" || chatID == "" {
		logger.WarnCF("mcpserver", "mcp.foruser.dropped",
			map[string]any{
				"tool":        toolName,
				"agent":       rec.agentID,
				"session_key": rec.sessionKey,
				"reason":      noChatReason(dest),
			})
		return
	}
	if constants.IsInternalChannel(channel) {
		// An internal turn (an asked one's, agency agent's): its output goes to
		// the asker or nowhere, never a chat.
		logger.InfoCF("mcpserver", "mcp.foruser.dropped",
			map[string]any{
				"tool":        toolName,
				"agent":       rec.agentID,
				"session_key": rec.sessionKey,
				"reason":      "internal_channel",
				"channel":     channel,
			})
		return
	}

	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Content: result.ForUser,
	}); err != nil {
		logger.WarnCF("mcpserver", "mcp.foruser.publish_failed",
			map[string]any{
				"tool":        toolName,
				"agent":       rec.agentID,
				"session_key": rec.sessionKey,
				"channel":     channel,
				"error":       err.Error(),
			})
		return
	}
	logger.InfoCF("mcpserver", "mcp.foruser.delivered",
		map[string]any{
			"tool":        toolName,
			"agent":       rec.agentID,
			"session_key": rec.sessionKey,
			"channel":     channel,
			"content_len": len(result.ForUser),
		})
}

// noChatReason names, for the log, why a call had no chat to send to.
func noChatReason(dest tools.ReplyChat) string {
	switch {
	case dest.Ambiguous:
		return "several_turns_running"
	case dest.Channel != "":
		return "turn_has_no_chat"
	}
	return "no_turn_running"
}
