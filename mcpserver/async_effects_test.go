// Agency
// License: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// A background tool's late result answers the chat of the turn the call was
// made in: when it arrives after that turn ended, and when another turn of
// the session is running then. The fixture was recorded before the host
// adopted the SDK's mcphost endpoints and must not change.

// laterAsync is a background tool whose callback the test fires later, on
// a context of its own (as agent_spawn's task runner does).
type laterAsync struct {
	wireTool
	mu  sync.Mutex
	cbs []toolkit.AsyncCallback
}

func (l *laterAsync) ExecuteAsync(_ context.Context, _ map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	l.mu.Lock()
	l.cbs = append(l.cbs, cb)
	l.mu.Unlock()
	return toolkit.NewResult("started")
}

func (l *laterAsync) fire(i int, text string) {
	l.mu.Lock()
	cb := l.cbs[i]
	l.mu.Unlock()
	cb(context.Background(), &toolkit.Result{ForLLM: text, ForUser: text + " (for the user)"})
}

func TestAsyncResultChatUnchanged(t *testing.T) {
	var logs bytes.Buffer
	restore := logger.RedirectForTest(&logs)
	defer restore()

	const session = "agent:alice:main"
	bg := &laterAsync{}
	bg.wireTool = wireTool{name: "bg_later", desc: "Background", params: map[string]any{}, run: wireText("sync")}
	msgBus := bus.NewMessageBus()
	store := NewSessionTokenStore()
	tok := store.Issue("alice", session, "/tmp/archive/alice")
	srv, err := New(
		WithSessionTokenStore(store),
		WithAgentRegistries(map[string]*tools.ToolRegistry{"alice": newRegistryWith(bg)}),
		WithAllowlist([]string{"*"}),
		WithMessageBus(msgBus),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()
	call := func() {
		body, merr := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "bg_later", "arguments": map[string]any{"session_token": tok}},
		})
		if merr != nil {
			t.Fatal(merr)
		}
		req, rerr := http.NewRequest(http.MethodPost, ts.URL+InternalEndpointPath, bytes.NewReader(body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, derr := ts.Client().Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatal(cerr)
		}
	}

	type delivery struct {
		Case     string                `json:"case"`
		Outbound []bus.OutboundMessage `json:"outbound"`
		Inbound  []bus.InboundMessage  `json:"inbound"`
	}
	var got []delivery
	drain := func(name string) {
		d := delivery{Case: name}
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			m, ok := msgBus.SubscribeOutbound(ctx)
			cancel()
			if !ok {
				break
			}
			d.Outbound = append(d.Outbound, m)
		}
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			m, ok := msgBus.ConsumeInbound(ctx)
			cancel()
			if !ok {
				break
			}
			d.Inbound = append(d.Inbound, m)
		}
		got = append(got, d)
	}

	// The turn the call is made in ends before the result arrives.
	end := store.BeginTurn(session, "telegram", "chat-1")
	call()
	end()
	bg.fire(0, "first result")
	drain("after the turn ended")

	// Another turn of the session, in another chat, runs when it arrives.
	end = store.BeginTurn(session, "telegram", "chat-1")
	call()
	end()
	endOther := store.BeginTurn(session, "telegram", "chat-2")
	bg.fire(1, "second result")
	endOther()
	drain("while another turn runs")

	var lines []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil {
			continue
		}
		if msg, ok := fields["message"].(string); ok && strings.HasPrefix(msg, "mcp.") {
			delete(fields, "time")
			delete(fields, "caller")
			norm, nerr := json.Marshal(fields)
			if nerr != nil {
				t.Fatal(nerr)
			}
			lines = append(lines, string(norm))
		}
	}
	out, err := json.MarshalIndent(map[string]any{"deliveries": got, "logs": lines}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	path := filepath.Join("testdata", "async_effects.json")
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
		t.Errorf("a background result's delivery changed:\nwant %s\n got %s", want, out)
	}
}
