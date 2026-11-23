package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acplib "github.com/a3tai/openclaw-go/acp"
	"github.com/a3tai/openclaw-go/protocol"
	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/test/stubprovider"
	"go.bison9.com/agency/utils"
)

// chatState returns the runs that ended with a chat event of state, and the
// text of each.
func (l *eventLog) chatState(state string) map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string)
	for _, ev := range l.events {
		if ev.EventName != protocol.EventChat {
			continue
		}
		var p struct {
			RunID   string `json:"runId"`
			State   string `json:"state"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil && p.State == state {
			out[p.RunID] = p.Message.Text
		}
	}
	return out
}

// The adapter's chat.abort reaches the device channel with the node's "main"
// session: with nothing running it is a successful no-op, and with the
// bridge's run open it sends the agent a /cancel in that session.
func TestAbortSessionKeyClientChatAbort(t *testing.T) {
	g := startACPTestGateway(t, t.TempDir(), false)
	conn, err := g.connect(t, true, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { utils.CloseQuietly(conn.client) })
	client := &abortSessionKeyClient{Client: conn.client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.ChatAbort(ctx, protocol.ChatAbortParams{RunID: "nothing"}); err != nil {
		t.Fatalf("chat.abort with nothing running: %v", err)
	}

	if _, err := client.ChatSend(ctx, protocol.ChatSendParams{SessionKey: "main", Message: "work", IdempotencyKey: "run-1"}); err != nil {
		t.Fatalf("chat.send: %v", err)
	}
	in, ok := g.bus.ConsumeInbound(ctx)
	if !ok || in.Content != "work" {
		t.Fatalf("inbound = %+v", in)
	}
	if err := client.ChatAbort(ctx, protocol.ChatAbortParams{RunID: "run-1"}); err != nil {
		t.Fatalf("chat.abort: %v", err)
	}
	cancelMsg, ok := g.bus.ConsumeInbound(ctx)
	if !ok || cancelMsg.Content != "/cancel" || cancelMsg.ChatID != in.ChatID ||
		cancelMsg.Metadata["session_key"] != in.Metadata["session_key"] || cancelMsg.Metadata["session_key"] == "" {
		t.Fatalf("abort inbound = %+v, want /cancel in the run's chat and session (%q)", cancelMsg, in.Metadata["session_key"])
	}
}

// acpClient drives an ACP server over pipes as an editor would.
type acpClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
}

func (c *acpClient) send(id any, method string, params any) {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		msg["id"] = id
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c.in, "%s\n", raw); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
}

// response waits for the response to request id, skipping notifications.
func (c *acpClient) response(id float64) map[string]any {
	c.t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case m := <-c.lines:
			if got, ok := m["id"].(float64); ok && got == id {
				return m
			}
		case <-timeout:
			c.t.Fatalf("no response to request %v", id)
		}
	}
}

// Cancelling in an ACP client while a prompt runs stops the agent's turn:
// the bridge reads session/cancel at once, the prompt returns with the
// cancelled stop reason, the device channel ends the run as aborted and the
// agent's /cancel reply says it stopped the turn.
func TestACPCancelStopsTheAgentRun(t *testing.T) {
	g := startACPTestGateway(t, t.TempDir(), false)
	stub := stubprovider.New(t)
	stub.SetDefault(stubprovider.Hang(0))

	cfg := &config.Config{
		Providers: []modelspec.Provider{{Name: "stub", Protocol: "openai-chat", BaseURL: stub.URL(), APIKey: "test-key"}},
		Models:    []modelspec.Model{{ModelName: "primary", Model: "stub-model", Provider: "stub", Enabled: true}},
		Agents: config.AgentsConfig{
			BaseDir: t.TempDir(),
			Defaults: config.AgentDefaults{
				Models: []string{"primary"}, MaxTokens: 512, MaxToolIterations: 5,
				RequestTimeout: 20, ProgressInterval: -1,
			},
			List: []config.AgentConfig{{ID: "main", Name: "Main", Default: true}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	al, err := agent.NewAgentLoop(ctx, cfg, g.bus)
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	cm, cmErr := channels.NewManager(&config.Config{}, g.bus, nil)
	if cmErr != nil {
		t.Fatalf("channels.NewManager: %v", cmErr)
	}
	cm.RegisterChannel("device", g.channel)
	al.SetChannelManager(cm)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if runErr := al.Run(ctx); runErr != nil {
			t.Errorf("AgentLoop.Run: %v", runErr)
		}
	}()
	// The agent's replies go to the device channel (the channel manager's
	// own dispatch is not started here).
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			out, ok := g.bus.SubscribeOutbound(ctx)
			if !ok {
				return
			}
			if out.Channel != "device" {
				continue
			}
			if sendErr := g.channel.Send(ctx, out); sendErr != nil && ctx.Err() == nil {
				t.Errorf("device Send: %v", sendErr)
			}
		}
	}()
	t.Cleanup(func() {
		al.Stop()
		cancel()
		<-runDone
		<-pumpDone
		waitCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if waitErr := al.WaitTurns(waitCtx); waitErr != nil {
			t.Errorf("turns still running: %v", waitErr)
		}
		al.Close(context.Background())
	})

	events := &eventLog{}
	var bridge atomic.Pointer[acpBridge]
	wsURL, tlsConfig := g.connectArgs()
	conn, err := connectBridge(ctx, g.dataDir, wsURL, acpTestToken, tlsConfig, true, func(ev protocol.Event) {
		events.add(ev)
		if br := bridge.Load(); br != nil {
			br.handleGatewayEvent(ev)
		}
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { utils.CloseQuietly(conn.client) })
	br := newACPBridge(&abortSessionKeyClient{Client: conn.client})
	bridge.Store(br)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	server := acplib.NewServer(br, acpInput(inR, func(n acplib.CancelNotification) { br.Cancel(ctx, n) }), outW)
	br.setNotifier(server)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if serveErr := server.Serve(ctx); serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Errorf("Serve: %v", serveErr)
		}
	}()
	client := &acpClient{t: t, in: inW, lines: make(chan map[string]any, 64)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				client.lines <- m
			}
		}
	}()
	t.Cleanup(func() {
		utils.CloseQuietly(inW)
		<-serveDone
		utils.CloseQuietly(outW)
	})

	client.send(1, "session/new", map[string]any{"cwd": "/", "mcpServers": []any{}})
	newResult, ok := client.response(1)["result"].(map[string]any)
	sid, isString := newResult["sessionId"].(string)
	if !ok || !isString || sid == "" {
		t.Fatal("session/new returned no sessionId")
	}
	client.send(2, "session/prompt", map[string]any{"sessionId": sid, "prompt": []map[string]any{{"type": "text", "text": "a long job"}}})
	waitFor(t, "the model request", func() bool { return stub.Hanging() == 1 })

	client.send(nil, "session/cancel", map[string]any{"sessionId": sid})
	resp := client.response(2)
	result, ok := resp["result"].(map[string]any)
	if !ok || resp["error"] != nil || result["stopReason"] != acplib.StopReasonCancelled {
		t.Fatalf("prompt response = %v, want stopReason cancelled", resp)
	}

	waitFor(t, "the model request to be abandoned", func() bool { return stub.Hanging() == 0 })
	var promptRun, stopped string
	waitFor(t, "the aborted run and the /cancel reply", func() bool {
		aborted, finals := events.chatState("aborted"), events.chatState("final")
		promptRun, stopped = "", ""
		for id := range aborted {
			promptRun = id
		}
		for _, text := range finals {
			stopped = text
		}
		return promptRun != "" && stopped != ""
	})
	if n := len(events.chatState("aborted")); n != 1 {
		t.Fatalf("%d runs aborted, want 1", n)
	}
	if finals := events.chatState("final"); len(finals) != 1 || !strings.HasPrefix(stopped, "Main: stopped") {
		t.Fatalf("final replies = %v, want one /cancel reply saying Main stopped", finals)
	}
	if _, ok := events.chatState("final")[promptRun]; ok {
		t.Fatal("the aborted run also got a final reply")
	}
	if n := stub.Count(); n != 1 {
		t.Fatalf("model requests = %d, want 1 (no retry after the cancel)", n)
	}
}
