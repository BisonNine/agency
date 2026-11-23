// ClawEh
// License: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// The tests drive the host through its HTTP handler, the two endpoints a
// client reaches.

// rpc posts one JSON-RPC request to path (bearer, when set, is the
// Authorization: Bearer token) on ctx and returns the HTTP status and body.
func rpc(ctx context.Context, h http.Handler, path, bearer, method string, params any) (int, []byte) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// callResult is a decoded tools/call response.
type callResult struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

func (r callResult) text() string {
	var b strings.Builder
	for _, c := range r.Result.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// errPanicked is the answer a call gets when its tool panicked: MCP's
// internal error (-32603), as mcp-go's recovery gives it.
type errPanicked struct{ msg string }

// callOn calls tool with args on srv's /internal endpoint. A protocol error
// from a tool panic is raised again as a panic, as the old dispatch did; any
// other protocol error fails the test.
func callOn(ctx context.Context, t *testing.T, srv *MCPServer, tool string, args map[string]any) (string, bool) {
	t.Helper()
	out, isErr, err := callRaw(ctx, srv, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	return out, isErr
}

func callRaw(ctx context.Context, srv *MCPServer, tool string, args map[string]any) (string, bool, error) {
	if args == nil {
		args = map[string]any{}
	}
	_, raw := rpc(ctx, srv.httpServer.Handler, InternalEndpointPath, "", "tools/call",
		map[string]any{"name": tool, "arguments": args})
	var res callResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("decode tools/call response: %w\nbody: %s", err, raw)
	}
	if res.Error != nil {
		if res.Error.Code == -32603 && strings.HasPrefix(res.Error.Message, "panic recovered") {
			panic(errPanicked{res.Error.Message})
		}
		return "", false, fmt.Errorf("tools/call returned a protocol error: %+v\nbody: %s", *res.Error, raw)
	}
	return res.text(), res.Result.IsError, nil
}

// toolOutputKey carries, for dispatchToolCall, the tool-output switch the
// server is built with (WithToolOutput).
type toolOutputKey struct{}

func withToolOutput(ctx context.Context, on func() bool) context.Context {
	return context.WithValue(ctx, toolOutputKey{}, on)
}

// stubRegistryID is the agent whose registry, in dispatchToolCall, publishes
// a tool no real agent has, so a call of it reaches the host's checks. It
// sorts after every test agent, so it never describes their tools.
const stubRegistryID = "~stub"

// dispatchToolCall makes one tools/call on /internal of a host serving the
// token's agent (from resolver) with policy, msgBus and toolActivity, as a
// CLI provider's call arrives. tracker, when set, is the host's first-call
// tracker.
func dispatchToolCall(
	ctx context.Context,
	toolName string,
	args map[string]any,
	sessionTokens *SessionTokenStore,
	resolver AgentResolver,
	tracker *firstCallTracker,
	policy acl.Policy,
	msgBus *bus.MessageBus,
	toolActivity ToolActivityNotifier,
) (string, bool) {
	regs := map[string]*tools.ToolRegistry{}
	if tok, ok := args[sessionTokenParam].(string); ok && sessionTokens != nil {
		if rec, found := sessionTokens.Resolve(tok); found && resolver != nil {
			if reg, ok := resolver(rec.agentID); ok && reg != nil {
				regs[rec.agentID] = reg
			}
		}
	}
	if !published(regs, toolName) {
		regs[stubRegistryID] = newRegistryWith(&mockTool{name: toolName, params: map[string]any{}, result: toolkit.NewResult("stub")})
	}
	opts := []Option{
		WithAgentRegistries(regs),
		WithAllowlist([]string{"*"}),
		WithACLPolicy(policy),
		WithMessageBus(msgBus),
		WithToolActivityNotifier(toolActivity),
	}
	if resolver != nil {
		opts = append(opts, WithAgentLookup(resolver))
	}
	if sessionTokens != nil {
		opts = append(opts, WithSessionTokenStore(sessionTokens))
	}
	if on, ok := ctx.Value(toolOutputKey{}).(func() bool); ok {
		opts = append(opts, WithToolOutput(on))
	}
	srv, err := New(opts...)
	if err != nil {
		panic(err)
	}
	defer func() {
		if serr := srv.Shutdown(context.WithoutCancel(ctx)); serr != nil {
			panic(serr)
		}
	}()
	if tracker != nil {
		srv.tracker = tracker
	}
	if sessionTokens == nil {
		srv.sessionTokens = nil
	}
	callArgs := maps.Clone(args)
	out, isErr, err := callRaw(ctx, srv, toolName, callArgs)
	if err != nil {
		panic(err)
	}
	return out, isErr
}

// published reports whether some registry has a tool published as name.
func published(regs map[string]*tools.ToolRegistry, name string) bool {
	for _, reg := range regs {
		if _, ok := reg.Get(name); ok {
			return true
		}
		for _, internal := range reg.List() {
			if tl, ok := reg.Get(internal); ok {
				if en, ok := tl.(toolkit.ExternalNamer); ok && en.ExternalName() == name {
					return true
				}
			}
		}
	}
	return false
}

// listedTool is one entry of a tools/list result.
type listedTool struct {
	Tool struct {
		Name           string          `json:"name"`
		Description    string          `json:"description"`
		RawInputSchema json.RawMessage `json:"inputSchema"`
	}
}

// listTools returns what tools/list publishes on path (with bearer as the
// Authorization: Bearer token on /mcp), by name.
func listTools(t *testing.T, srv *MCPServer, path, bearer string) map[string]listedTool {
	t.Helper()
	code, raw := rpc(context.Background(), srv.httpServer.Handler, path, bearer, "tools/list", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("tools/list on %s: HTTP %d: %s", path, code, raw)
	}
	var res struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode tools/list: %v\n%s", err, raw)
	}
	out := make(map[string]listedTool, len(res.Result.Tools))
	for _, rawTool := range res.Result.Tools {
		var lt listedTool
		if err := json.Unmarshal(rawTool, &lt.Tool); err != nil {
			t.Fatal(err)
		}
		out[lt.Tool.Name] = lt
	}
	return out
}

// hostFor builds a host over regs with every tool visible on both endpoints
// and st as its token store.
func hostFor(t *testing.T, regs map[string]*tools.ToolRegistry, st *SessionTokenStore, opts ...Option) *MCPServer {
	t.Helper()
	all := append([]Option{WithAgentRegistries(regs), WithAllowlist([]string{"*"}), WithSessionTokenStore(st)}, opts...)
	srv, err := New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return srv
}

// postBody posts a raw JSON-RPC body to path and returns the response body.
func postBody(srv *MCPServer, path, bearer string, body []byte) ([]byte, error) {
	var msg struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	_, raw := rpc(context.Background(), srv.httpServer.Handler, path, bearer, msg.Method, msg.Params)
	return raw, nil
}

// sessionTokenParam is the argument that carries the session token on
// /internal (mcphost.TokenArgument).
const sessionTokenParam = "session_token"

// AgentResolver returns the per-agent tool registry for a given agent name,
// or (nil, false) when the agent is unknown.
type AgentResolver func(agentName string) (*tools.ToolRegistry, bool)
