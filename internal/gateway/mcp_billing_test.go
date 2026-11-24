// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/mcpserver"
	"go.bison9.com/agency/servicetoken"
)

// freeAddr is a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// mcpRPC posts one JSON-RPC request to the host and returns its result.
func mcpRPC(t *testing.T, url, bearer, method string, params any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Error != nil {
		t.Fatalf("%s on %s: %s (%v)", method, url, raw, err)
	}
	return out.Result
}

// billingServed checks that both endpoints list the billing tool and answer
// a call of it for alice's conversation.
func billingServed(t *testing.T, base string, sessionToken, serviceToken string) {
	t.Helper()
	for _, ep := range []struct{ path, bearer string }{
		{mcpserver.InternalEndpointPath, ""},
		{"/mcp", serviceToken},
	} {
		var list struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(mcpRPC(t, base+ep.path, ep.bearer, "tools/list", map[string]any{}), &list); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range list.Tools {
			found = found || tool.Name == "billing"
		}
		if !found {
			t.Errorf("%s does not list the billing tool", ep.path)
		}
		args := map[string]any{}
		if ep.bearer == "" {
			args["session_token"] = sessionToken
		}
		var call struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(mcpRPC(t, base+ep.path, ep.bearer, "tools/call", map[string]any{"name": "billing", "arguments": args}), &call); err != nil {
			t.Fatal(err)
		}
		if call.IsError || len(call.Content) == 0 || !strings.HasPrefix(call.Content[0].Text, "Billing code: ") {
			t.Errorf("%s: billing call = %+v", ep.path, call)
		}
	}
}

// An agent with default settings is served the billing tool on both MCP
// endpoints, as a CLI model reaches it, and still after a reload.
func TestMCPHost_ServesBillingByDefault(t *testing.T) {
	RegisterToolProvidersForTest()
	t.Setenv(global.EnvVarHome, t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Agents.BaseDir = t.TempDir()
	cfg.Agents.Defaults.Models = []string{"test-model"}
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}}
	cfg.MCPHost.Enabled = true
	cfg.MCPHost.Listen = freeAddr(t)

	msgBus := bus.NewMessageBus()
	al, err := agent.NewAgentLoop(t.Context(), cfg, msgBus)
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	t.Cleanup(func() { al.Close(context.Background()) })
	const svcToken = "SVC-alice-plain"
	if err := servicetoken.Save(servicetoken.Path(cfg.DataDir()), map[string]string{"alice": svcToken}); err != nil {
		t.Fatal(err)
	}

	services := &gatewayServices{fatal: newFatalNotifier(nil)}
	if err := startMCPServer(cfg, al, msgBus, services); err != nil {
		t.Fatalf("start: %v", err)
	}
	tok := services.SessionTokens.Issue("alice", "agent:alice:main", t.TempDir())
	base := "http://" + cfg.MCPHost.Listen
	billingServed(t, base, tok, svcToken)

	// The reload: the host stops, the agents are rebuilt, the host starts again.
	if err := services.MCPServer.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := al.ReloadConfig(context.Background(), cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := startMCPServer(cfg, al, msgBus, services); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() {
		if err := services.MCPServer.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	billingServed(t, base, tok, svcToken)
}
