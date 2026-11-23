// ClawEh
// License: MIT

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.bison9.com/agent/clock"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	clawmcp "go.bison9.com/agency/mcp"
)

// mcpEdgeServer serves an in-process MCP server with the named no-op tools.
func mcpEdgeServer(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	srv := server.NewMCPServer("cov", "0.0.1")
	for _, n := range toolNames {
		srv.AddTool(mcp.NewTool(n, mcp.WithDescription("no-op")),
			func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts
}

// mcpEdgeHeldServer is an MCP server whose first request (the first connect's
// handshake) is held until release is closed and then refused. Requests that
// arrive while it is held are served; once it is released every new request is
// refused, so the held connect fails outright.
type mcpEdgeHeldServer struct {
	ts       *httptest.Server
	held     chan struct{} // closed when the first request arrives
	release  chan struct{}
	once     sync.Once
	released atomic.Bool
}

func newMCPEdgeHeldServer(t *testing.T, toolName string) *mcpEdgeHeldServer {
	t.Helper()
	srv := server.NewMCPServer("held", "0.0.1")
	srv.AddTool(mcp.NewTool(toolName, mcp.WithDescription("no-op")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	inner := server.NewStreamableHTTPServer(srv)
	h := &mcpEdgeHeldServer{held: make(chan struct{}), release: make(chan struct{})}
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := false
		h.once.Do(func() { first = true })
		if first {
			close(h.held)
			<-h.release
			h.released.Store(true)
		}
		if first || h.released.Load() {
			http.Error(w, "refused", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		h.ts.CloseClientConnections()
		h.ts.Close()
	})
	return h
}

// mcpEdgeHost signals each catalogue refresh.
type mcpEdgeHost struct{ refreshed chan struct{} }

func (h *mcpEdgeHost) RefreshCatalogue() {
	select {
	case h.refreshed <- struct{}{}:
	default:
	}
}

// mcpEdgeLoop builds a single-agent loop ("main") with the given mcp_tools and
// servers, not yet initialized, and a captured log.
func mcpEdgeLoop(t *testing.T, mcpTools []string, servers map[string]config.MCPServerConfig) (*AgentLoop, *safeBufLoop) {
	t.Helper()
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	cfg := toolRegTestConfig(t)
	cfg.Agents.List[0].MCPTools = mcpTools
	cfg.Tools.MCP.Servers = servers
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	t.Cleanup(func() { al.Close(context.Background()) })
	return al, logs
}

// The background retry loop connects a desired server the manager has not
// connected (here one a reload is still connecting), registers its tools on
// the agents and refreshes the host; a reload whose reconcile failed is
// logged; turning every server off tears the manager down.
func TestLoopMCP_ReloadAndRetryLifecycle(t *testing.T) {
	svc := mcpEdgeServer(t, "ping")
	late := newMCPEdgeHeldServer(t, "pong")
	al, logs := mcpEdgeLoop(t, []string{"svc", "late"}, map[string]config.MCPServerConfig{
		"svc": {Enabled: false, Type: "http", URL: svc.URL},
	})
	host := &mcpEdgeHost{refreshed: make(chan struct{}, 1)}
	al.SetMCPHost(host)
	fc := clock.NewFake(time.Now())
	al.clock = fc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retryDone := make(chan struct{})
	go func() {
		defer close(retryDone)
		al.mcpRetryLoop(ctx)
	}()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	if err := fc.BlockUntil(waitCtx, 1); err != nil {
		t.Fatalf("retry loop never started its ticker: %v", err)
	}

	// MCP off: no manager, no status; a tick finds nothing to retry.
	if st := al.MCPStatus(); st != nil {
		t.Fatalf("MCPStatus with MCP off = %v, want nil", st)
	}
	fc.Advance(mcpRetryInterval)

	// First enable on reload: a fresh connect registers svc's tools.
	cfg := al.GetConfig()
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"svc": {Enabled: true, Type: "http", URL: svc.URL},
	}
	al.ReinitMCP(context.Background())
	assertHasTool(t, agentToolNames(t, al), "mcp_svc_ping")
	if st := al.MCPStatus(); len(st) != 1 || st[0].Name != "svc" {
		t.Fatalf("MCPStatus = %+v, want svc", st)
	}

	// A reload adds "late"; its connect is held, so it is desired but not
	// connected when the retry loop ticks.
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"svc":  {Enabled: true, Type: "http", URL: svc.URL},
		"late": {Enabled: true, Type: "http", URL: late.ts.URL},
	}
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		al.ReinitMCP(context.Background())
	}()
	select {
	case <-late.held:
	case <-waitCtx.Done():
		t.Fatal("the reload never tried to connect late")
	}
	fc.Advance(mcpRetryInterval)
	select {
	case <-host.refreshed:
	case <-waitCtx.Done():
		t.Fatal("the retry loop never refreshed the host")
	}
	assertHasTool(t, agentToolNames(t, al), "mcp_late_pong")

	// The reload's own connect of late is refused: it says so and keeps the
	// retry loop's connection and tools.
	close(late.release)
	<-reloadDone
	if !strings.Contains(logs.String(), "Some MCP servers failed to reconcile on reload") {
		t.Error("the failed reconcile was not logged")
	}
	names := agentToolNames(t, al)
	assertHasTool(t, names, "mcp_svc_ping")
	assertHasTool(t, names, "mcp_late_pong")

	// Every server off: the manager is closed and the tools go with the
	// rebuilt registry on a real reload; here the manager is gone.
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"svc": {Enabled: false, Type: "http", URL: svc.URL},
	}
	al.ReinitMCP(context.Background())
	if al.mcp.hasManager() {
		t.Error("manager kept after every server was turned off")
	}
	if err := al.EnsureMCPInitialized(context.Background()); err != nil {
		t.Errorf("init error after turning MCP off = %v", err)
	}

	cancel()
	<-retryDone
}

// Closing the loop stops the retry loop.
func TestLoopMCP_RetryLoopStopsOnClose(t *testing.T) {
	al, _ := mcpEdgeLoop(t, nil, nil)
	fc := clock.NewFake(time.Now())
	al.clock = fc
	done := make(chan struct{})
	go func() {
		defer close(done)
		al.mcpRetryLoop(context.Background())
	}()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	if err := fc.BlockUntil(waitCtx, 1); err != nil {
		t.Fatalf("retry loop never started: %v", err)
	}
	al.Close(context.Background())
	select {
	case <-done:
	case <-waitCtx.Done():
		t.Fatal("retry loop still running after Close")
	}
	if fc.Waiters() != 0 {
		t.Errorf("ticker not stopped: %d waiters", fc.Waiters())
	}
}

// A server that fails its initial connect is not fatal: the manager stays up
// (so the retry loop can recover it) and the failure is logged.
func TestLoopMCP_InitialConnectFailureKeepsManager(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	al, logs := mcpEdgeLoop(t, []string{"down"}, map[string]config.MCPServerConfig{
		"down": {Enabled: true, Type: "http", URL: down.URL},
	})
	if err := al.EnsureMCPInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureMCPInitialized = %v, want nil", err)
	}
	if !al.mcp.hasManager() {
		t.Fatal("manager dropped after a failed initial connect")
	}
	if !strings.Contains(logs.String(), "Some MCP servers failed initial connect; retrying in background") {
		t.Error("the failed initial connect was not logged")
	}
	if slices.ContainsFunc(agentToolNames(t, al), func(n string) bool { return strings.HasPrefix(n, "mcp_") }) {
		t.Error("MCP tools registered for a server that is down")
	}
}

// The agent's mcp_tools allow-list admits tools one by one.
func TestLoopMCP_AllowListGatesPerTool(t *testing.T) {
	svc := mcpEdgeServer(t, "ping", "pong")
	al, _ := mcpEdgeLoop(t, []string{"svc_ping"}, map[string]config.MCPServerConfig{
		"svc": {Enabled: true, Type: "http", URL: svc.URL},
	})
	if err := al.EnsureMCPInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureMCPInitialized: %v", err)
	}
	names := agentToolNames(t, al)
	assertHasTool(t, names, "mcp_svc_ping")
	if slices.Contains(names, "mcp_svc_pong") {
		t.Errorf("mcp_svc_pong registered though mcp_tools admits only svc_ping: %v", names)
	}
}

// Refreshing a server whose name prefixes several others keeps all of them,
// the longest-named server's tools winning a name both produce.
func TestLoopMCP_RefreshKeepsNestedSiblings(t *testing.T) {
	svc := mcpEdgeServer(t, "ping")
	svcA := mcpEdgeServer(t, "x")
	svcAB := mcpEdgeServer(t, "y")
	al, _ := mcpEdgeLoop(t, []string{"svc", "svc_a", "svc_a_b"}, map[string]config.MCPServerConfig{
		"svc":     {Enabled: true, Type: "http", URL: svc.URL},
		"svc_a":   {Enabled: true, Type: "http", URL: svcA.URL},
		"svc_a_b": {Enabled: true, Type: "http", URL: svcAB.URL},
	})
	if err := al.EnsureMCPInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureMCPInitialized: %v", err)
	}
	al.refreshMCPServerTools(al.mcp.peekManager(), "svc")
	names := agentToolNames(t, al)
	for _, want := range []string{"mcp_svc_ping", "mcp_svc_a_x", "mcp_svc_a_b_y"} {
		assertHasTool(t, names, want)
	}
}

// A tools-changed event from a manager that is no longer the live one changes
// nothing and does not refresh the host.
func TestLoopMCP_StaleManagerIgnored(t *testing.T) {
	svc := mcpEdgeServer(t, "ping")
	al, _ := mcpEdgeLoop(t, []string{"svc"}, map[string]config.MCPServerConfig{
		"svc": {Enabled: true, Type: "http", URL: svc.URL},
	})
	if err := al.EnsureMCPInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureMCPInitialized: %v", err)
	}
	host := &mcpEdgeHost{refreshed: make(chan struct{}, 1)}
	al.SetMCPHost(host)

	al.refreshMCPServerTools(clawmcp.NewManager(), "svc")

	assertHasTool(t, agentToolNames(t, al), "mcp_svc_ping")
	select {
	case <-host.refreshed:
		t.Error("a stale manager refreshed the host")
	default:
	}
}

// A server that comes back from a reconnect with a changed tool list has its
// tools re-registered on the agents through the manager's tools-changed
// handler, and the host follows.
func TestLoopMCP_ToolsChangedHandlerReregisters(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	srv, ts := newRefreshTestServer(t)
	al := newMCPAgentLoop(t, ts.URL)
	host := &mcpEdgeHost{refreshed: make(chan struct{}, 1)}
	al.SetMCPHost(host)

	srv.AddTool(mcp.NewTool("pong", mcp.WithDescription("no-op")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	// The manager's own reconnect, not RefreshMCPServer: only the handler
	// re-registers.
	if err := al.mcp.peekManager().Reconnect(context.Background(), "svc"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case <-host.refreshed:
	case <-waitCtx.Done():
		t.Fatal("the tools-changed handler never refreshed the host")
	}
	names := agentToolNames(t, al)
	assertHasTool(t, names, "mcp_svc_ping")
	assertHasTool(t, names, "mcp_svc_pong")
}
