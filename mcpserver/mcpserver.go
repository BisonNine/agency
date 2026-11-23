// ClawEh
// License: MIT

// Package mcpserver exposes claw host-side tools to MCP-compatible clients
// over a Streamable HTTP transport. It lets CLI providers (claude-cli,
// codex-cli, antigravity-cli, cursor-cli) natively call claw tools via MCP — the correct
// alternative to having the CLI emit tool-call JSON in its prose (which
// created infinite outer loops, since CLIs are themselves agentic).
//
// The server exposes two endpoints over the same registries, ACL, and token
// store, differing only in how the session token (SST<64hex>) is transported:
//
//   - /internal — every tool carries a required `session_token` parameter. This
//     is ClawEh's multi-assistant routing surface: one configured endpoint, a
//     different token per call. ClawEh's own CLI providers point here.
//   - /mcp — the standard bearer endpoint: clean tool schemas (no session_token
//     parameter) with auth via `Authorization: Bearer <token>`. A missing or
//     invalid bearer is rejected at the HTTP layer with a 401. This is the
//     universal surface for external MCP clients and the probe test suite.
//
// Either way the token resolves to an (agentID, sessionKey) pair and is the sole
// auth mechanism: it identifies both the calling agent and the active session.
// If the token is missing, malformed, unknown, or the sub-agent sentinel, the
// call fails closed.
//
// The MCP mechanism (serving, the session_token argument, dispatch with its
// callbacks, published names, tools/list_changed) is the agent SDK's mcphost;
// this package is the policy: the listener and paths, who a token is (session
// and service tokens), the visibility filters and the ACL, the per-call turn
// scope, and what a call publishes on the bus (host.go).
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/mcphost"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// DefaultListen is the default bind address for the MCP server.
const DefaultListen = "127.0.0.1:5911"

// DefaultEndpointPath is the default HTTP endpoint for the standard, bearer-token
// MCP surface. Tools here have clean schemas; auth is Authorization: Bearer.
const DefaultEndpointPath = "/mcp"

// InternalEndpointPath is the HTTP endpoint for ClawEh's multi-assistant
// routing: every tool carries the in-call session_token parameter. This is the
// endpoint ClawEh's own CLI providers point at.
const InternalEndpointPath = "/internal"

// MCPServer serves claw tools on two mcphost endpoints. tools/list on
// /internal is the union of every agent registry, since tool names/schemas
// are not sensitive; on /mcp it is the bearer's agent's own tools. A
// tools/call resolves its token to an agent identity, the per-agent ACL gates
// the (agent, tool) pair, and the agent's own registry executes.
type MCPServer struct {
	// alerter hears when the listener dies after startup and no onServeError
	// handler is installed; nothing restarts it.
	alerter alerter.Alerter
	// onServeError, when set, is handed a Serve error after startup instead of
	// the alerter: the gateway uses it to shut the process down. phase
	// decides, once, whether a Serve error belongs to Start (phaseFailed: Start
	// returns it, nothing else hears of it) or to the running server
	// (phaseServing: handed off or alerted).
	onServeError    func(error)
	phase           atomic.Int32
	agentRegistries map[string]*tools.ToolRegistry // agentID → registry (dispatch target + schema source)
	// agentLookup resolves a tools/call for an agent not in agentRegistries:
	// a temporary agent created after the host started. Never part of the
	// published catalogue.
	agentLookup   func(agentID string) (*tools.ToolRegistry, bool)
	internalAllow []string // tools/list visibility filter for /internal
	externalAllow []string // tools/list visibility filter for /mcp (bearer)

	// NOTE: progressive tool discovery is intentionally NOT applied on the host.
	// It is an in-loop model-context optimization only; the MCP host always
	// advertises the full allowed catalogue and never hides a tool from a dispatch, so an
	// external client gets every authorized tool and runs its own tool handling.
	listen       string
	endpointPath string // bearer endpoint (/mcp)
	internalPath string // session-token-parameter endpoint (/internal)

	sessionTokens *SessionTokenStore   // SST-prefixed per-session tokens for session-scoped tools
	workspaces    map[string]string    // agentID → workspace (for boot/first-call logging)
	policy        acl.Policy           // per-agent tools/call ACL; defaults to acl.Default
	msgBus        *bus.MessageBus      // outbound publish target for tool ForUser payloads (optional)
	toolActivity  ToolActivityNotifier // "/tools on" breadcrumb renderer for MCP-routed calls (optional)
	toolOutput    func() bool          // whether a tool's own output goes to the chat (optional; nil = never)
	// taskRegistrar is the process's task registry as an agent sees it,
	// put on each call of a session token (optional).
	taskRegistrar func(agentID string) hook.TaskRegistrar

	httpServer *http.Server
	tracker    *firstCallTracker
	// netListen binds the listener (net.Listen; a test seam).
	netListen func(network, address string) (net.Listener, error)

	// internal and bearer are the two endpoints (/internal and /mcp), served
	// by the agent SDK's mcphost over the same registries, ACL and tokens.
	internal, bearer *mcphost.Endpoint
}

// ToolActivityNotifier renders the one-line "/tools on" breadcrumb for a tool
// call about to be dispatched over the MCP path, or "" to emit nothing (tool
// activity off for the session, unknown agent, or no summary). Rendering and the
// per-session toggle live behind this callback (in the agent loop); the MCP
// server only publishes the returned line to the session's originating user.
type ToolActivityNotifier func(agentID, sessionKey, toolName string, args map[string]any) string

// Option configures an MCPServer.
type Option func(*MCPServer)

// WithAgentRegistries sets the per-agent tool registries. The same map
// drives both the global tools/list catalogue (union of all registries,
// deduped by name) and the tools/call dispatch target (keyed by the
// agent ID that the token manager resolves to).
func WithAgentRegistries(registries map[string]*tools.ToolRegistry) Option {
	return func(m *MCPServer) {
		if len(registries) == 0 {
			m.agentRegistries = nil
			return
		}
		m.agentRegistries = make(map[string]*tools.ToolRegistry, len(registries))
		maps.Copy(m.agentRegistries, registries)
	}
}

// WithAgentLookup sets the fallback that resolves the tool registry of an
// agent the registries map does not hold (a temporary agent created at run
// time, whose session token the agent loop issues), for tools/call dispatch.
func WithAgentLookup(fn func(agentID string) (*tools.ToolRegistry, bool)) Option {
	return func(m *MCPServer) { m.agentLookup = fn }
}

// WithTaskRegistrar sets the process's task registry as the agent agentID
// sees it (a clone sees its source's tasks), which a session token's tool
// calls carry, as a turn's in-process calls do.
func WithTaskRegistrar(fn func(agentID string) hook.TaskRegistrar) Option {
	return func(m *MCPServer) { m.taskRegistrar = fn }
}

// WithAgentWorkspaces supplies the agentID → workspace map. Used only
// for boot-log assertions and the first-MCP-call-per-agent log entry.
func WithAgentWorkspaces(ws map[string]string) Option {
	return func(m *MCPServer) {
		if len(ws) == 0 {
			m.workspaces = nil
			return
		}
		m.workspaces = make(map[string]string, len(ws))
		maps.Copy(m.workspaces, ws)
	}
}

// WithListen sets the listen address (default: 127.0.0.1:5911).
func WithListen(addr string) Option {
	return func(m *MCPServer) {
		if addr != "" {
			m.listen = addr
		}
	}
}

// WithEndpointPath sets the bearer (standard) HTTP endpoint path (default:
// /mcp). The session-token-parameter endpoint is always served at /internal.
func WithEndpointPath(path string) Option {
	return func(m *MCPServer) {
		if path != "" {
			m.endpointPath = path
		}
	}
}

// WithInternalAllowlist sets the tools/list visibility filter for the /internal
// endpoint. Patterns are matched by config.MatchVisibility (equality-or-prefix,
// underscores collapsed, leading mcp_ stripped; "*" = all). An empty/nil list
// means no tools are exposed on /internal (fail-closed). Every tool obeys the
// filter; nothing is hard-excluded.
func WithInternalAllowlist(names []string) Option {
	return func(m *MCPServer) {
		if len(names) == 0 {
			m.internalAllow = nil
			return
		}
		m.internalAllow = append([]string(nil), names...)
	}
}

// WithExternalAllowlist sets the tools/list visibility filter for the bearer
// endpoint (/mcp). Same matching semantics as WithInternalAllowlist.
func WithExternalAllowlist(names []string) Option {
	return func(m *MCPServer) {
		if len(names) == 0 {
			m.externalAllow = nil
			return
		}
		m.externalAllow = append([]string(nil), names...)
	}
}

// WithAllowlist applies the same visibility filter to BOTH endpoints. Convenience
// for callers/tests that don't need per-endpoint lists.
func WithAllowlist(names []string) Option {
	return func(m *MCPServer) {
		WithInternalAllowlist(names)(m)
		WithExternalAllowlist(names)(m)
	}
}

// WithMessageBus supplies the outbound message bus. When set, MCP-routed tool
// dispatch publishes any non-Silent ForUser payload from a tool result to the
// originating user's channel/chatID (looked up from the session record). When
// nil or unset, ForUser content from MCP-routed tool calls is silently dropped
// with a log line; the MCP response envelope is unaffected.
func WithMessageBus(b *bus.MessageBus) Option {
	return func(m *MCPServer) { m.msgBus = b }
}

// WithToolActivityNotifier supplies the "/tools on" breadcrumb renderer. When set
// (and a message bus is present), each MCP-routed tool call publishes the returned
// one-liner to the session's originating user before the tool runs — so tool calls
// made by CLI providers surface the same way loop-dispatched ones do. When nil or
// the notifier returns "", nothing is published.
func WithToolActivityNotifier(n ToolActivityNotifier) Option {
	return func(m *MCPServer) { m.toolActivity = n }
}

// WithToolOutput supplies the switch for a tool's own output (ForUser): it is
// sent to the chat of the call's turn only while on reports true, read on
// every call, as the agent loop does with agents.defaults.stream_tool_activity.
// When nil, it is never sent. Background results are not affected.
func WithToolOutput(on func() bool) Option {
	return func(m *MCPServer) { m.toolOutput = on }
}

// WithAlerter routes a listener death after startup to an alerter.
func WithAlerter(a alerter.Alerter) Option {
	return func(m *MCPServer) { m.alerter = a }
}

// WithOnServeError hands a listener death after startup to fn instead of the
// alerter. The error names the listen address. The gateway installs its
// fail-fast path here so a dead MCP host takes the process down for the
// service manager to restart.
func WithOnServeError(fn func(error)) Option {
	return func(m *MCPServer) { m.onServeError = fn }
}

// WithSessionTokenStore makes the server use s instead of a store of its own.
// The store is process-lifetime: the server is rebuilt on every config reload,
// but the tokens already rendered into running prompts (Maestro workers, turns
// in flight) are not, so they must keep resolving. A nil s is ignored.
func WithSessionTokenStore(s *SessionTokenStore) Option {
	return func(m *MCPServer) {
		if s != nil {
			m.sessionTokens = s
		}
	}
}

// WithACLPolicy installs a per-agent ACL policy consulted on every
// tools/call after token validation. tools/list is never gated by this.
// A nil argument is treated as acl.Default (open by default).
func WithACLPolicy(p acl.Policy) Option {
	return func(m *MCPServer) { m.policy = p }
}

// New constructs an MCPServer. An agent-token manager and at least one
// agent registry are required; the agent registries double as the schema
// source for tools/list (union of all of them, deduped by name).
func New(opts ...Option) (*MCPServer, error) {
	m := &MCPServer{
		listen:        DefaultListen,
		endpointPath:  DefaultEndpointPath,
		internalPath:  InternalEndpointPath,
		sessionTokens: newSessionTokenStore(),
		netListen:     net.Listen,
	}
	for _, opt := range opts {
		opt(m)
	}

	if len(m.agentRegistries) == 0 {
		return nil, errors.New("mcpserver: at least one agent registry is required")
	}
	if m.policy == nil {
		m.policy = acl.Default
	}

	// Boot-log assertion: emit one line per registered agent so any
	// mis-bindings are visible at startup.
	for agentID, reg := range m.agentRegistries {
		ws := m.workspaces[agentID]
		logger.InfoCF("mcpserver", "Agent registry bound",
			map[string]any{
				"agent":     agentID,
				"workspace": ws,
				"tools":     reg.Count(),
			})
	}

	m.tracker = newFirstCallTracker(m.workspaces)

	// /internal — session-token parameter on every tool (ClawEh's CLI providers).
	internal, err := mcphost.New(newEndpointHost(m, false, m.internalAllow).config())
	if err != nil {
		return nil, fmt.Errorf("mcpserver: internal endpoint: %w", err)
	}
	// /mcp — standard bearer endpoint, clean tool schemas (probe / external MCP).
	// The bearer token names the caller, so tools/list offers only what that
	// agent may call, described as that agent's tools.
	bearer, err := mcphost.New(newEndpointHost(m, true, m.externalAllow).config())
	if err != nil {
		internal.Close()
		return nil, fmt.Errorf("mcpserver: bearer endpoint: %w", err)
	}
	m.internal, m.bearer = internal, bearer

	mux := http.NewServeMux()
	mux.Handle(m.internalPath, internal)
	mux.Handle(m.endpointPath, bearer)
	m.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	return m, nil
}

// registryFor resolves the tool registry a tools/call for agentName dispatches
// to: a registered agent's, else the lookup's (a temporary agent).
func (m *MCPServer) registryFor(agentName string) (*tools.ToolRegistry, bool) {
	if reg, ok := m.agentRegistries[agentName]; ok {
		return reg, true
	}
	if m.agentLookup != nil {
		return m.agentLookup(agentName)
	}
	return nil, false
}

// Listen returns the configured listen address.
func (m *MCPServer) Listen() string { return m.listen }

// EndpointPath returns the configured endpoint path.
func (m *MCPServer) EndpointPath() string { return m.endpointPath }

// SessionTokens returns the session token store. Callers (e.g. the AgentLoop)
// use it to issue tokens when a new session context manager is created and to
// revoke tokens on session clear or eviction.
func (m *MCPServer) SessionTokens() *SessionTokenStore { return m.sessionTokens }

// RefreshCatalogue brings each endpoint's published tools in step with the
// agent registries (mcphost.Endpoint.Refresh): connected clients get
// tools/list_changed when what an endpoint publishes changed, so an external
// client picks the change up without reconnecting. The agent loop calls it
// after an external MCP server's tools are re-registered.
func (m *MCPServer) RefreshCatalogue() {
	ctx := context.Background()
	added, deleted := 0, 0
	for _, ep := range []*mcphost.Endpoint{m.internal, m.bearer} {
		r := ep.Refresh(ctx)
		added += len(r.Added)
		deleted += len(r.Removed)
	}
	if added > 0 || deleted > 0 {
		logger.InfoCF("mcpserver", "MCP host catalogue refreshed",
			map[string]any{"added": added, "deleted": deleted})
	}
}

// Start begins serving in a background goroutine. It returns after the
// listener is bound (binding failures are returned immediately), so callers
// can rely on the server being ready once Start returns nil.
func (m *MCPServer) Start() error {
	ln, err := m.netListen("tcp", m.listen)
	if err != nil {
		return fmt.Errorf("mcpserver: bind %s: %w", m.listen, err)
	}

	logger.InfoCF("mcpserver", "MCP server starting",
		map[string]any{
			"listen":   m.listen,
			"endpoint": m.endpointPath, // bearer (standard)
			"internal": m.internalPath, // session-token parameter
			"agents":   len(m.agentRegistries),
		})

	errCh := make(chan error, 1)
	go func() {
		err := m.httpServer.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		if m.phase.CompareAndSwap(phaseStarting, phaseFailed) {
			// Still inside Start's window: Start returns this error.
			errCh <- err
			return
		}
		if m.onServeError != nil {
			m.onServeError(fmt.Errorf("%s: %w", m.listen, err))
			return
		}
		logger.ErrorCF("mcpserver", "MCP server exited",
			map[string]any{"error": err.Error()})
		if m.alerter != nil {
			m.alerter.Send(alerter.Alert{
				Title:       "MCP host server stopped",
				Description: m.listen + ": external MCP clients and CLI providers lose the host tools until " + app.ShortName() + " is restarted",
				Details:     err.Error(),
				EventID:     "mcpserver",
			})
		}
	}()

	// Give the server a brief window to fail fast.
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("mcpserver: start failed: %w", err)
		}
		return nil
	case <-time.After(150 * time.Millisecond):
		if m.phase.CompareAndSwap(phaseStarting, phaseServing) {
			return nil
		}
		// Serve failed just as the window closed; the error is Start's.
		return fmt.Errorf("mcpserver: start failed: %w", <-errCh)
	}
}

// Start phases (MCPServer.phase).
const (
	phaseStarting int32 = iota
	phaseServing
	phaseFailed
)

// Shutdown stops the MCP server. It waits briefly for any in-flight tool
// dispatches (POST requests) to complete, then force-closes all remaining
// connections — including the persistent SSE GET connections that CLI clients
// hold open indefinitely. Without the force-close step, http.Server.Shutdown
// blocks for the full context deadline (typically 30 s) waiting for those
// idle SSE connections to drain on their own.
func (m *MCPServer) Shutdown(ctx context.Context) error {
	logger.InfoCF("mcpserver", "MCP server shutting down", nil)

	if m.httpServer != nil {
		// Stop accepting new connections immediately.
		if err := m.httpServer.Close(); err != nil {
			logger.WarnCF("mcpserver", "Failed to close MCP HTTP server", map[string]any{"error": err.Error()})
		}
	}

	// Wait up to 3 s for in-flight tool dispatches to finish.
	deadline := time.Now().Add(3 * time.Second)
	for m.inFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if remaining := m.inFlight(); remaining > 0 {
		logger.WarnCF("mcpserver", "Shutdown with active dispatches still in flight",
			map[string]any{"count": remaining})
	}

	m.internal.Close()
	m.bearer.Close()
	return nil
}

// inFlight is the number of tool calls running on both endpoints.
func (m *MCPServer) inFlight() int { return m.internal.InFlight() + m.bearer.InFlight() }

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
