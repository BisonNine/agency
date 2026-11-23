// ClawEh
// License: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/mcpserver/acl"
	"go.bison9.com/agency/tools"
)

// What an MCP tool call does besides its answer: the messages it publishes on
// the bus (the tool's own output, the "/tools on" breadcrumb, a background
// result's delivery and re-injection), the audit rows, the session's tool
// statistics and the host's log lines. The fixture was recorded before the
// host adopted the SDK's mcphost endpoints and must not change.

const effectsLeak = "SST" + "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"

// effectsAsync is a background tool: it reports its result through the
// callback before it returns.
type effectsAsync struct{ wireTool }

func (e *effectsAsync) ExecuteAsync(ctx context.Context, _ map[string]any, cb tools.AsyncCallback) *tools.ToolResult {
	cb(ctx, &tools.ToolResult{ForLLM: "background done", ForUser: "background for the user"})
	return tools.NewToolResult("started in the background")
}

type effectsRecord struct {
	Responses []string              `json:"responses"`
	Outbound  []bus.OutboundMessage `json:"outbound"`
	Inbound   []bus.InboundMessage  `json:"inbound"`
	Audit     []string              `json:"audit"`
	Tally     []any                 `json:"tally"`
	Logs      []string              `json:"logs"`
}

func TestCallEffectsUnchanged(t *testing.T) {
	if err := audit.Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	restore := logger.RedirectForTest(&logs)
	defer restore()

	const session = "agent:alice:main"
	regs := map[string]*tools.ToolRegistry{
		"alice": newRegistryWith(
			&wireTool{name: "write_note", desc: "Writes a note", params: map[string]any{}, run: func(context.Context, map[string]any) *tools.ToolResult {
				return &tools.ToolResult{ForLLM: "saved " + effectsLeak, ForUser: "Note saved"}
			}},
			&wireTool{name: "denied_tool", desc: "Denied", params: map[string]any{}, run: wireText("never")},
			&effectsAsync{wireTool{name: "bg_tool", desc: "Background", params: map[string]any{}, run: wireText("sync")}},
		),
	}
	msgBus := bus.NewMessageBus()
	store := NewSessionTokenStore()
	tok := store.Issue("alice", session, "/tmp/archive/alice")
	store.SetDepth(session, 1)
	end := store.BeginTurn(session, "telegram", "chat-1")
	defer end()

	srv, err := New(
		WithSessionTokenStore(store),
		WithAgentRegistries(regs),
		WithAgentWorkspaces(map[string]string{"alice": "/ws/alice"}),
		WithAllowlist([]string{"*"}),
		WithMessageBus(msgBus),
		WithToolActivityNotifier(func(agentID, sessionKey, toolName string, _ map[string]any) string {
			return "tool " + toolName + " for " + agentID
		}),
		WithToolOutput(func() bool { return true }),
		WithACLPolicy(acl.PolicyFunc(func(_, tool string) bool { return tool != "denied_tool" })),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	tools.BeginToolStats(session)
	var rec effectsRecord
	post := func(path, bearer, tool string, args map[string]any) {
		body, perr := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args},
		})
		if perr != nil {
			t.Fatal(perr)
		}
		req, perr := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
		if perr != nil {
			t.Fatal(perr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, perr := ts.Client().Do(req)
		if perr != nil {
			t.Fatal(perr)
		}
		raw, perr := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil && perr == nil {
			perr = cerr
		}
		if perr != nil {
			t.Fatal(perr)
		}
		rec.Responses = append(rec.Responses, string(raw))
	}
	post(InternalEndpointPath, "", "write_note", map[string]any{"session_token": tok})
	post(DefaultEndpointPath, tok, "write_note", map[string]any{})
	post(InternalEndpointPath, "", "bg_tool", map[string]any{"session_token": tok})
	post(InternalEndpointPath, "", "denied_tool", map[string]any{"session_token": tok})
	post(InternalEndpointPath, "", "write_note", map[string]any{"session_token": "SST" + strings.Repeat("0", 64)})

	calls, errs, last := tools.EndToolStats(session)
	rec.Tally = []any{calls, errs, last}

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		m, ok := msgBus.SubscribeOutbound(ctx)
		cancel()
		if !ok {
			break
		}
		rec.Outbound = append(rec.Outbound, m)
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		m, ok := msgBus.ConsumeInbound(ctx)
		cancel()
		if !ok {
			break
		}
		rec.Inbound = append(rec.Inbound, m)
	}
	events, err := audit.Default().Query(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		rec.Audit = append(rec.Audit, e.Kind+" "+e.Agent+" "+e.Tool+" "+e.Outcome)
	}
	// Each record at INFO and above, without its time and call site.
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil || fields["level"] == "debug" {
			continue
		}
		delete(fields, "time")
		delete(fields, "caller")
		norm, merr := json.Marshal(fields) // map keys sorted
		if merr != nil {
			t.Fatal(merr)
		}
		rec.Logs = append(rec.Logs, string(norm))
	}

	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	path := filepath.Join("testdata", "call_effects.json")
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
		t.Errorf("an MCP call's effects changed:\nwant %s\n got %s", want, out)
	}
}
