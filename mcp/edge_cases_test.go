package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"go.bison9.com/agency/config"
)

// closeManager closes mgr at the end of the test.
func closeManager(t *testing.T, mgr *Manager) {
	t.Helper()
	t.Cleanup(func() {
		if err := mgr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

func TestLoadEnvFile_InvalidContent(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty key", content: "A=1\n=value\n", want: "invalid format at line 2: empty key"},
		{name: "line too long", content: "A=" + strings.Repeat("x", 70*1024) + "\n", want: "error reading env file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".env")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := loadEnvFile(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) || got != nil {
				t.Fatalf("loadEnvFile = %v, %v; want an error containing %q", got, err, tt.want)
			}
		})
	}
}

func TestLoadFromConfig_ConnectsConfiguredServers(t *testing.T) {
	good := newTestMCPServer(t)
	mgr := NewManager()
	closeManager(t, mgr)
	cfg := &config.Config{}
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"svc": {Enabled: true, URL: good.URL},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	if got := storedToolNames(t, mgr, "svc"); len(got) != 1 || got[0] != "ping" {
		t.Fatalf("tools = %v, want [ping]", got)
	}
}

// One failing server does not fail the load while another connects.
func TestLoadFromMCPConfig_PartialFailure(t *testing.T) {
	good := newTestMCPServer(t)
	mgr := NewManager()
	closeManager(t, mgr)
	err := mgr.LoadFromMCPConfig(context.Background(), config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"good": {Enabled: true, URL: good.URL},
		"bad":  {Enabled: true, Type: "carrier-pigeon"},
	}}, t.TempDir())
	if err != nil {
		t.Fatalf("LoadFromMCPConfig = %v, want nil while one server connects", err)
	}
	if _, ok := mgr.GetServer("good"); !ok {
		t.Fatal("good should be connected")
	}
	if _, ok := mgr.GetServer("bad"); ok {
		t.Fatal("bad should not be connected")
	}
	if msg, _ := mgr.lastError("bad"); !strings.Contains(msg, "unsupported transport type: carrier-pigeon") {
		t.Fatalf("bad's last error = %q", msg)
	}
}

func TestConnectServer_InvalidConfig(t *testing.T) {
	closed := httptest.NewServer(nil)
	closed.Close()
	tests := []struct {
		name string
		cfg  config.MCPServerConfig
		want string
	}{
		{name: "neither url nor command", cfg: config.MCPServerConfig{}, want: "either URL or command must be provided"},
		{name: "http without url", cfg: config.MCPServerConfig{Type: "http"}, want: "URL is required for http transport"},
		{name: "http bad url", cfg: config.MCPServerConfig{Type: "http", URL: "http://[::1"}, want: "failed to create HTTP client"},
		{name: "sse without url", cfg: config.MCPServerConfig{Type: "sse"}, want: "URL is required for sse transport"},
		{name: "sse bad url", cfg: config.MCPServerConfig{Type: "sse", URL: "http://[::1"}, want: "failed to create SSE client"},
		{
			name: "sse server down",
			cfg:  config.MCPServerConfig{Type: "sse", URL: closed.URL + "/sse", Headers: map[string]string{"X-Key": "v"}},
			want: "failed to start transport",
		},
		{name: "stdio without command", cfg: config.MCPServerConfig{Type: "stdio"}, want: "command is required for stdio transport"},
		{
			name: "stdio env file missing",
			cfg:  config.MCPServerConfig{Command: "true", EnvFile: filepath.Join(t.TempDir(), "missing.env")},
			want: "failed to load env file",
		},
		{name: "unsupported type", cfg: config.MCPServerConfig{Type: "ftp"}, want: "unsupported transport type: ftp (supported: stdio, http, sse)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager()
			closeManager(t, mgr)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := mgr.ConnectServer(ctx, "svc", tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ConnectServer = %v, want %q", err, tt.want)
			}
			if _, ok := mgr.GetServer("svc"); ok {
				t.Fatal("a failed connect left a connection")
			}
			if msg, _ := mgr.lastError("svc"); msg != err.Error() {
				t.Fatalf("last error = %q, want the connect error", msg)
			}
		})
	}
}

// A shutdown that begins while a connect is in progress keeps the connect
// from installing the server, and is not recorded as a failure.
func TestConnectServer_ShutdownDuringConnect(t *testing.T) {
	mgr := NewManager()
	closeManager(t, mgr)
	hooks := &server.Hooks{}
	hooks.AddBeforeListTools(func(context.Context, any, *mcp.ListToolsRequest) { mgr.BeginShutdown() })
	srv := server.NewMCPServer("test", "0.0.1", server.WithHooks(hooks))
	srv.AddTool(mcp.NewTool("ping"), noopHandler)
	ts := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)

	err := mgr.ConnectServer(context.Background(), "svc", config.MCPServerConfig{Enabled: true, URL: ts.URL})
	if !errors.Is(err, errManagerStopping) {
		t.Fatalf("ConnectServer = %v, want errManagerStopping", err)
	}
	if _, ok := mgr.GetServer("svc"); ok {
		t.Fatal("a connect during shutdown installed the server")
	}
	if msg, _ := mgr.lastError("svc"); msg != "" {
		t.Fatalf("a shutdown was recorded as a failure: %q", msg)
	}
}

func TestCallTool_SuccessAndToolError(t *testing.T) {
	good := newTestMCPServer(t)
	mgr := NewManager()
	closeManager(t, mgr)
	ctx := context.Background()
	if err := mgr.ConnectServer(ctx, "svc", config.MCPServerConfig{Enabled: true, URL: good.URL, RevealTogether: true}); err != nil {
		t.Fatalf("ConnectServer: %v", err)
	}
	conn, _ := mgr.GetServer("svc")
	if !conn.RevealTogether() {
		t.Fatal("RevealTogether should follow the config")
	}

	res, err := mgr.CallTool(ctx, "svc", "ping", nil)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("CallTool(ping) = %+v, %v", res, err)
	}

	// A tool the server does not have is a tool error, not a dropped
	// connection: it is returned without reconnecting.
	_, err = mgr.CallTool(ctx, "svc", "missing", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "failed to call tool: ") {
		t.Fatalf("CallTool(missing) = %v, want a tool error", err)
	}
	if after, _ := mgr.GetServer("svc"); after != conn {
		t.Fatal("a tool error replaced the connection")
	}
}

func TestBeginShutdown_SecondCallIsNoOp(t *testing.T) {
	good := newTestMCPServer(t)
	mgr := NewManager()
	closeManager(t, mgr)
	mgr.probeInterval = time.Hour
	if err := mgr.ConnectServer(context.Background(), "svc", config.MCPServerConfig{Enabled: true, URL: good.URL}); err != nil {
		t.Fatalf("ConnectServer: %v", err)
	}
	mgr.BeginShutdown()
	mgr.BeginShutdown()
	if !mgr.stopping() {
		t.Fatal("manager should be stopping")
	}
	if conn, _ := mgr.GetServer("svc"); conn.probeStop != nil {
		t.Fatal("the probe was not stopped")
	}
}

func TestBusyServers_IncludesReconnecting(t *testing.T) {
	mgr := NewManager()
	mgr.trackInflight("b", 1)
	mgr.setReconnecting("a", true)
	mgr.setReconnecting("b", true)
	got := mgr.busyServers()
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("busyServers = %v, want [a b]", got)
	}
}

func TestStatus_ReconnectingTakesPrecedence(t *testing.T) {
	good := newTestMCPServer(t)
	mgr := NewManager()
	closeManager(t, mgr)
	if err := mgr.ConnectServer(context.Background(), "live", config.MCPServerConfig{Enabled: true, URL: good.URL}); err != nil {
		t.Fatalf("ConnectServer: %v", err)
	}
	mgr.setReconnecting("live", true)
	mgr.setReconnecting("down", true)
	mgr.recordFailure("down", errors.New("refused"))

	states := map[string]ServerStatus{}
	for _, st := range mgr.Status() {
		if _, dup := states[st.Name]; dup {
			t.Fatalf("%s listed twice", st.Name)
		}
		states[st.Name] = st
	}
	if st := states["live"]; st.State != StateReconnecting || st.Transport != "http" || st.ToolCount != 1 {
		t.Fatalf("live = %+v, want reconnecting over http with one tool", st)
	}
	if st := states["down"]; st.State != StateReconnecting || st.LastError != "refused" {
		t.Fatalf("down = %+v, want reconnecting (not cooldown) with its error", st)
	}
}

func TestEffectiveTransport(t *testing.T) {
	tests := []struct {
		cfg  config.MCPServerConfig
		want string
	}{
		{config.MCPServerConfig{Type: "sse", URL: "http://x"}, "sse"},
		{config.MCPServerConfig{URL: "http://x", Command: "c"}, "http"},
		{config.MCPServerConfig{Command: "c"}, "stdio"},
		{config.MCPServerConfig{}, ""},
	}
	for _, tt := range tests {
		if got := effectiveTransport(tt.cfg); got != tt.want {
			t.Errorf("effectiveTransport(%+v) = %q, want %q", tt.cfg, got, tt.want)
		}
	}
}

// A server removed from the config while down leaves no failure state.
func TestSync_ForgetsHealthOfRemovedServer(t *testing.T) {
	mgr := NewManager()
	closeManager(t, mgr)
	mgr.recordFailure("gone", errors.New("refused"))
	if err := mgr.Sync(context.Background(), config.MCPConfig{}, t.TempDir()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if msg, _ := mgr.lastError("gone"); msg != "" {
		t.Fatalf("last error kept: %q", msg)
	}
	for _, st := range mgr.Status() {
		if st.Name == "gone" {
			t.Fatalf("Status still lists the removed server: %+v", st)
		}
	}
}

func TestFormatDowntime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                             "45s",
		12*time.Minute + 30*time.Second:              "12m",
		2*time.Hour + 5*time.Minute + 30*time.Second: "2h5m",
	} {
		if got := formatDowntime(d); got != want {
			t.Errorf("formatDowntime(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestStderrTail_TruncatedSingleLineIsDropped(t *testing.T) {
	var tail stderrTail
	if _, err := tail.Write([]byte(strings.Repeat("x", stderrTailBytes+10))); err != nil {
		t.Fatal(err)
	}
	if got := tail.Lines(); got != nil {
		t.Fatalf("Lines = %q, want none: the only line was cut", got)
	}
}

func TestToolKey_UnencodableFallsBackToNameAndDescription(t *testing.T) {
	tool := mcp.Tool{Name: "a", Description: "d"}
	tool.InputSchema.Properties = map[string]any{"f": func() {}}
	if got := toolKey(tool); got != "a\x00d" {
		t.Fatalf("toolKey = %q", got)
	}
}

// Tool-list refreshes are ignored once the manager is closed, for a
// connection that is not the live one, and when the re-list fails.
func TestToolRefresh_IgnoredWhenStale(t *testing.T) {
	srv, ts := newTestMCPServerHandle(t)
	mgr, changed := newChangeCapturingManager(t)
	ctx := context.Background()
	if err := mgr.ConnectServer(ctx, "svc", config.MCPServerConfig{Enabled: true, URL: ts.URL}); err != nil {
		t.Fatalf("ConnectServer: %v", err)
	}
	conn, _ := mgr.GetServer("svc")
	other := &client.Client{}

	// The server's list changes; a notification for a client that is not the
	// live connection, or a server that is not connected, re-lists nothing.
	srv.AddTool(mcp.NewTool("pong"), noopHandler)
	drain(changed)
	mgr.onToolsListChanged("svc", other)
	mgr.onToolsListChanged("missing", conn.Client)
	if got := storedToolNames(t, mgr, "svc"); strings.Join(got, ",") != "ping" {
		t.Fatalf("tools = %v, want [ping]", got)
	}

	// A failed re-list keeps the stored list, though the server's has changed.
	if err := conn.Client.Close(); err != nil {
		t.Fatal(err)
	}
	mgr.refreshTools(ctx, "svc", conn.Client, "test")
	if got := storedToolNames(t, mgr, "svc"); strings.Join(got, ",") != "ping" {
		t.Fatalf("tools after a failed re-list = %v, want [ping]", got)
	}

	// Once closed nothing is replaced and no handler fires.
	mgr.closed.Store(true)
	if mgr.replaceToolsIfChanged("svc", conn.Client, []mcp.Tool{mcp.NewTool("x")}, "test") {
		t.Fatal("replaceToolsIfChanged changed a closed manager")
	}
	mgr.onToolsListChanged("svc", conn.Client)
	mgr.notifyToolsChanged("svc")
	select {
	case name := <-changed:
		t.Fatalf("handler fired for %q", name)
	default:
	}
	mgr.closed.Store(false)
	mgr.probeOnce("missing") // not connected: nothing to probe
}

// drain empties ch of anything already delivered.
func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
