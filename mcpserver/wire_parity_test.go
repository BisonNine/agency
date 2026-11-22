// ClawEh
// License: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agency/agenttoken"
	"go.bison9.com/agency/internal/tokenhash"
	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// The MCP host's wire behaviour, as a client sees it, on both endpoints:
// the initialize result, tools/list for each agent, and a representative set
// of tools/call results (success, tool error, nil result, panic, redaction,
// published-name collision, session key, ACL and grant refusals, unknown
// tool, missing, bad, revoked and sub-agent tokens, a service token) with
// the HTTP status and the headers a client reads. The fixture was recorded
// before the host adopted the SDK's mcphost and must not change: run with
// UPDATE_WIRE=1 only to add a case, never to accept a change.

const (
	wireAliceToken = "SST" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	wireBobToken   = "SST" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	wireGoneToken  = "SST" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	wireBadToken   = "SST" + "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	wireBobService = "bob-service-token"
	wireLeaked     = "SST" + "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
)

// wireTool is a fixture tool whose behaviour is a function of its call.
type wireTool struct {
	name, desc string
	params     map[string]any
	run        func(ctx context.Context, args map[string]any) *tools.ToolResult
}

func (w *wireTool) Name() string               { return w.name }
func (w *wireTool) Description() string        { return w.desc }
func (w *wireTool) Parameters() map[string]any { return w.params }
func (w *wireTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	return w.run(ctx, args)
}

// wireExternal is a wireTool published under another name, as an upstream
// MCP tool is.
type wireExternal struct {
	wireTool
	external string
}

func (w *wireExternal) ExternalName() string { return w.external }

// wireScoped is a session-scoped wireTool.
type wireScoped struct{ wireTool }

func (*wireScoped) IsSessionScoped() bool { return true }

func wireText(s string) func(context.Context, map[string]any) *tools.ToolResult {
	return func(context.Context, map[string]any) *tools.ToolResult { return tools.NewToolResult(s) }
}

// wireArgs reports the argument names a tool received.
func wireArgs(_ context.Context, args map[string]any) *tools.ToolResult {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return tools.NewToolResult("args=" + strings.Join(keys, ","))
}

// wireSession reports the session key on the call's context.
func wireSession(ctx context.Context, _ map[string]any) *tools.ToolResult {
	return tools.NewToolResult("session=" + tools.ToolSessionKey(ctx))
}

func wireRegistries() map[string]*tools.ToolRegistry {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"zone": map[string]any{"type": "string", "description": "Time zone"}},
		"required":   []any{"zone"},
	}
	open := map[string]any{
		"type":       "object",
		"properties": map[string]any{"meta": map[string]any{"type": "object"}},
	}
	alice := newRegistryWith(
		&wireTool{name: "time_now", desc: "Alice's clock", params: schema, run: wireArgs},
		&wireTool{name: "open_args", desc: "Free-form object", params: open, run: wireArgs},
		&wireTool{name: "fail_tool", desc: "Always fails", params: nil, run: func(context.Context, map[string]any) *tools.ToolResult {
			return tools.ErrorResult("boom")
		}},
		&wireTool{name: "nil_tool", desc: "Returns nothing", params: map[string]any{}, run: func(context.Context, map[string]any) *tools.ToolResult {
			return nil
		}},
		&wireTool{name: "panic_tool", desc: "Panics", params: map[string]any{}, run: func(context.Context, map[string]any) *tools.ToolResult {
			panic("wire fixture panic")
		}},
		&wireTool{name: "leak_tool", desc: "Echoes a token", params: map[string]any{}, run: wireText("token " + wireLeaked + " end")},
		&wireTool{name: "foruser_tool", desc: "Speaks to the user", params: map[string]any{}, run: func(context.Context, map[string]any) *tools.ToolResult {
			return &tools.ToolResult{ForLLM: "for the model", ForUser: "for the user"}
		}},
		&wireScoped{wireTool{name: "session_echo", desc: "Session-scoped", params: map[string]any{}, run: wireSession}},
		&wireTool{name: "ctx_probe", desc: "Not session-scoped", params: map[string]any{}, run: wireSession},
		&wireExternal{wireTool{name: "mcp_srv_read", desc: "Upstream read", params: map[string]any{}, run: wireText("upstream read")}, "srv_read"},
		// Two tools that want the published name srv_write.
		&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native write")},
		&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream write")}, "srv_write"},
	)
	bob := newRegistryWith(
		&wireTool{name: "time_now", desc: "Bob's clock", params: schema, run: wireArgs},
		&wireTool{name: "bob_only", desc: "Only Bob has it", params: map[string]any{}, run: wireText("bob only")},
		&wireTool{name: "fail_tool", desc: "Bob's failing tool", params: nil, run: wireText("bob's fail_tool ran")},
	)
	return map[string]*tools.ToolRegistry{"alice": alice, "bob": bob}
}

// wireServer builds the host over the fixture registries and tokens.
func wireServer(t *testing.T) http.Handler {
	t.Helper()
	store := NewSessionTokenStore()
	store.Register(wireAliceToken, "alice", "agent:alice:main", "/tmp/archive/alice")
	store.Register(wireBobToken, "bob", "agent:bob:main", "/tmp/archive/bob")
	store.Register(wireGoneToken, "alice", "agent:alice:gone", "/tmp/archive/alice")
	store.Revoke("agent:alice:gone")
	store.SyncServiceTokens(map[string]string{"bob": tokenhash.Hash(wireBobService)}, func(id string) string { return "/tmp/archive/" + id })

	srv, err := New(
		WithSessionTokenStore(store),
		WithAgentRegistries(wireRegistries()),
		WithInternalAllowlist([]string{"*"}),
		// /mcp leaves nil_tool and open_args unpublished.
		WithExternalAllowlist([]string{"time_now", "fail_tool", "panic_tool", "leak_tool", "foruser_tool", "session_echo", "ctx_probe", "srv", "bob_only"}),
		WithACLPolicy(acl.PolicyFunc(func(agent, tool string) bool { return agent != "bob" || tool != "time_now" })),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.httpServer.Handler
}

// wireExchange is one request and what the client received.
type wireExchange struct {
	Name     string            `json:"name"`
	Path     string            `json:"path"`
	Auth     string            `json:"auth,omitempty"`
	Request  json.RawMessage   `json:"request"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	Response string            `json:"response"`
}

type wireCase struct {
	name, path, auth string
	method           string
	params           any
}

func wireCall(tool string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{"name": tool, "arguments": args}
}

func withToken(tok string, args map[string]any) map[string]any {
	out := map[string]any{"session_token": tok}
	maps.Copy(out, args)
	return out
}

func wireCases() []wireCase {
	initParams := map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "wire-fixture", "version": "1.0.0"},
	}
	var cases []wireCase
	// /internal: the token is an argument of each call.
	in := func(name, method string, params any) {
		cases = append(cases, wireCase{name: "internal " + name, path: InternalEndpointPath, method: method, params: params})
	}
	in("initialize", "initialize", initParams)
	in("tools/list", "tools/list", map[string]any{})
	in("call success", "tools/call", wireCall("time_now", withToken(wireAliceToken, map[string]any{"zone": "UTC"})))
	in("call open object args", "tools/call", wireCall("open_args", withToken(wireAliceToken, map[string]any{"meta": map[string]any{"k": "v"}})))
	in("call tool error", "tools/call", wireCall("fail_tool", withToken(wireAliceToken, nil)))
	in("call nil result", "tools/call", wireCall("nil_tool", withToken(wireAliceToken, nil)))
	in("call panic", "tools/call", wireCall("panic_tool", withToken(wireAliceToken, nil)))
	in("call redaction", "tools/call", wireCall("leak_tool", withToken(wireAliceToken, nil)))
	in("call foruser", "tools/call", wireCall("foruser_tool", withToken(wireAliceToken, nil)))
	in("call session-scoped", "tools/call", wireCall("session_echo", withToken(wireAliceToken, nil)))
	in("call not session-scoped", "tools/call", wireCall("ctx_probe", withToken(wireAliceToken, nil)))
	in("call external name", "tools/call", wireCall("srv_read", withToken(wireAliceToken, nil)))
	in("call internal name of external tool", "tools/call", wireCall("mcp_srv_read", withToken(wireAliceToken, nil)))
	in("call colliding name", "tools/call", wireCall("srv_write", withToken(wireAliceToken, nil)))
	in("call internal name of shadowed tool", "tools/call", wireCall("mcp_srv_write", withToken(wireAliceToken, nil)))
	in("call ACL refusal", "tools/call", wireCall("time_now", withToken(wireBobToken, map[string]any{"zone": "UTC"})))
	in("call tool not granted", "tools/call", wireCall("leak_tool", withToken(wireBobToken, nil)))
	in("call other agent's same-named tool", "tools/call", wireCall("fail_tool", withToken(wireBobToken, nil)))
	in("call service token", "tools/call", wireCall("bob_only", withToken(wireBobService, nil)))
	in("call service token hash refused", "tools/call", wireCall("bob_only", withToken(tokenhash.Hash(wireBobService), nil)))
	in("call unknown tool", "tools/call", wireCall("no_such_tool", withToken(wireAliceToken, nil)))
	in("call missing token", "tools/call", wireCall("time_now", map[string]any{"zone": "UTC"}))
	in("call non-string token", "tools/call", wireCall("time_now", map[string]any{"session_token": 42}))
	in("call bad token", "tools/call", wireCall("time_now", withToken(wireBadToken, nil)))
	in("call revoked token", "tools/call", wireCall("time_now", withToken(wireGoneToken, nil)))
	in("call sub-agent token", "tools/call", wireCall("time_now", withToken(agenttoken.SubagentSentinel, nil)))

	// /mcp: the token is the bearer header.
	bearer := func(name, auth, method string, params any) {
		cases = append(cases, wireCase{name: "bearer " + name, path: DefaultEndpointPath, auth: auth, method: method, params: params})
	}
	bearer("initialize without a token", "", "initialize", initParams)
	bearer("initialize with a bad token", "Bearer "+wireBadToken, "initialize", initParams)
	bearer("initialize with a revoked token", "Bearer "+wireGoneToken, "initialize", initParams)
	bearer("initialize with the sub-agent token", "Bearer "+agenttoken.SubagentSentinel, "initialize", initParams)
	bearer("initialize with another scheme", "Basic "+wireAliceToken, "initialize", initParams)
	bearer("initialize", "Bearer "+wireAliceToken, "initialize", initParams)
	bearer("tools/list alice", "Bearer "+wireAliceToken, "tools/list", map[string]any{})
	bearer("tools/list bob", "Bearer "+wireBobToken, "tools/list", map[string]any{})
	bearer("tools/list bob service token", "Bearer "+wireBobService, "tools/list", map[string]any{})
	bearer("call success", "bearer "+wireAliceToken, "tools/call", wireCall("time_now", map[string]any{"zone": "UTC"}))
	bearer("call with a session_token argument", "Bearer "+wireAliceToken, "tools/call", wireCall("time_now", withToken(wireBobToken, map[string]any{"zone": "UTC"})))
	bearer("call tool error", "Bearer "+wireAliceToken, "tools/call", wireCall("fail_tool", nil))
	bearer("call panic", "Bearer "+wireAliceToken, "tools/call", wireCall("panic_tool", nil))
	bearer("call redaction", "Bearer "+wireAliceToken, "tools/call", wireCall("leak_tool", nil))
	bearer("call session-scoped", "Bearer "+wireAliceToken, "tools/call", wireCall("session_echo", nil))
	bearer("call not session-scoped", "Bearer "+wireAliceToken, "tools/call", wireCall("ctx_probe", nil))
	bearer("call colliding name", "Bearer "+wireAliceToken, "tools/call", wireCall("srv_write", nil))
	bearer("call unpublished tool", "Bearer "+wireAliceToken, "tools/call", wireCall("nil_tool", nil))
	bearer("call ACL refusal", "Bearer "+wireBobToken, "tools/call", wireCall("time_now", map[string]any{"zone": "UTC"}))
	bearer("call tool not granted", "Bearer "+wireBobToken, "tools/call", wireCall("leak_tool", nil))
	bearer("call service token", "Bearer "+wireBobService, "tools/call", wireCall("bob_only", nil))
	bearer("call without a token", "", "tools/call", wireCall("time_now", nil))
	return cases
}

func TestWireParity(t *testing.T) {
	ts := httptest.NewServer(wireServer(t))
	defer ts.Close()

	cases := wireCases()
	got := make([]wireExchange, 0, len(cases))
	for i, c := range cases {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": c.method, "params": c.params})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+c.path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		raw, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatalf("%s: read: %v", c.name, err)
		}
		headers := map[string]string{}
		for _, h := range []string{"Content-Type", "WWW-Authenticate", "Mcp-Session-Id"} {
			if v := resp.Header.Get(h); v != "" {
				headers[h] = v
			}
		}
		got = append(got, wireExchange{
			Name: c.name, Path: c.path, Auth: redactAuth(c.auth), Request: body,
			Status: resp.StatusCode, Headers: headers, Response: string(raw),
		})
	}

	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	path := filepath.Join("testdata", "wire_parity.json")
	if os.Getenv("UPDATE_WIRE") == "1" {
		if werr := os.WriteFile(path, out, 0o644); werr != nil {
			t.Fatal(werr)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, out) {
		var w []wireExchange
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for i := range max(len(w), len(got)) {
			switch {
			case i >= len(w):
				t.Errorf("extra exchange %q", got[i].Name)
			case i >= len(got):
				t.Errorf("missing exchange %q", w[i].Name)
			default:
				a, aerr := json.Marshal(w[i])
				b, berr := json.Marshal(got[i])
				if aerr != nil || berr != nil {
					t.Fatalf("%s: encode: %v %v", w[i].Name, aerr, berr)
				}
				if !bytes.Equal(a, b) {
					t.Errorf("%s:\nwant %s\n got %s", w[i].Name, a, b)
				}
			}
		}
	}
}

// redactAuth names the credential a fixture request carried without
// repeating it.
func redactAuth(h string) string {
	if h == "" {
		return ""
	}
	scheme, tok, _ := strings.Cut(h, " ")
	for name, v := range map[string]string{
		"alice": wireAliceToken, "bob": wireBobToken, "revoked": wireGoneToken,
		"bad": wireBadToken, "bob-service": wireBobService, "sub-agent": agenttoken.SubagentSentinel,
	} {
		if tok == v {
			return fmt.Sprintf("%s <%s>", scheme, name)
		}
	}
	return scheme + " <other>"
}
