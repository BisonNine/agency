// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.bison9.com/agency/tools"
)

// TestBearerAuth covers the /mcp 401 gate: missing token, unknown token, the
// sub-agent sentinel, and a valid registered token (which reaches MCP). The
// header parsing itself is mcphost's (TestBearer_401 there).
func TestBearerAuth(t *testing.T) {
	st, tok := seedSessionToken("alice")
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(&mockTool{name: "read_file", params: map[string]any{}})}
	srv := hostFor(t, regs, st)
	init := map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"},
	}

	check := func(name, token string, wantCode int) {
		code, body := rpc(context.Background(), srv.httpServer.Handler, DefaultEndpointPath, token, "initialize", init)
		if code != wantCode {
			t.Errorf("%s: status = %d, want %d (%s)", name, code, wantCode, body)
		}
	}
	check("no token", "", http.StatusUnauthorized)
	check("unknown token", "SSTdeadbeef", http.StatusUnauthorized)
	check("valid token", tok, http.StatusOK)
}

// TestEndpointSchemas_DifferByParam asserts the two endpoints publish the same
// tool but only /internal carries the session_token parameter in its schema.
func TestEndpointSchemas_DifferByParam(t *testing.T) {
	rf := &mockTool{name: "read_file", params: map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
	}, result: tools.NewToolResult("ok")}
	regs := map[string]*tools.ToolRegistry{"alice": newRegistryWith(rf)}
	st, tok := seedSessionToken("alice")
	srv := hostFor(t, regs, st)

	schema := func(path, bearer string) string {
		tool, ok := listTools(t, srv, path, bearer)["read_file"]
		if !ok {
			t.Fatalf("read_file not published on %s", path)
		}
		return string(tool.Tool.RawInputSchema)
	}
	if internal := schema(InternalEndpointPath, ""); !strings.Contains(internal, sessionTokenParam) {
		t.Errorf("/internal schema missing %s param: %s", sessionTokenParam, internal)
	}
	if bearer := schema(DefaultEndpointPath, tok); strings.Contains(bearer, sessionTokenParam) {
		t.Errorf("/mcp schema must not carry %s param: %s", sessionTokenParam, bearer)
	}
}
