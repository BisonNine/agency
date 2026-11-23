package device

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"go.bison9.com/agency/gatewayproto"
	"go.bison9.com/agency/utils"
)

// abortAgent stands in for the agent layer behind a device connection: a
// message starts a turn that runs until a /cancel stops it, and a /cancel
// answers once, as the agent does ("Alice: stopped." when it stopped a turn).
type abortAgent struct {
	srv *Server
	url string

	mu      sync.Mutex
	turns   []inboundCall // every message handed to the agent layer
	running map[string]chan struct{}
	stopped []string // run ids of the turns a /cancel stopped
}

// inboundCall is one message the channel handed to the agent layer.
type inboundCall struct{ chatID, content, runID, sessionKey string }

func newAbortAgent(t *testing.T) (*abortAgent, string) {
	t.Helper()
	srv, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	srv.SetQuerier(&recordingQuerier{})
	a := &abortAgent{srv: srv, url: wsURL, running: make(map[string]chan struct{})}
	srv.SetInbound(func(_, chatID, content, runID, sessionKey string, _ []InboundAttachment) {
		a.mu.Lock()
		a.turns = append(a.turns, inboundCall{chatID, content, runID, sessionKey})
		if content != "/cancel" {
			a.running[runID] = make(chan struct{})
			a.mu.Unlock()
			return
		}
		reply := "No pending messages to cancel."
		for id, stop := range a.running {
			close(stop)
			a.stopped = append(a.stopped, id)
			delete(a.running, id)
			reply = "Alice: stopped."
		}
		a.mu.Unlock()
		if err := srv.DeliverReply(t.Context(), chatID, runID, reply, nil, ""); err != nil {
			t.Errorf("DeliverReply: %v", err)
		}
	})
	return a, wsURL
}

// calls returns the messages handed to the agent layer once there are n of
// them, or what there is after two seconds.
func (a *abortAgent) calls(n int) []inboundCall {
	return a.callsWithin(n, 2*time.Second)
}

// settled returns the messages handed to the agent layer once nothing more
// arrived for a short while, for checking that none did.
func (a *abortAgent) settled() []inboundCall {
	return a.callsWithin(-1, 200*time.Millisecond)
}

func (a *abortAgent) callsWithin(n int, wait time.Duration) []inboundCall {
	deadline := time.Now().Add(wait)
	for {
		a.mu.Lock()
		got := slices.Clone(a.turns)
		a.mu.Unlock()
		if (n >= 0 && len(got) >= n) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (a *abortAgent) stoppedRuns() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.stopped)
}

// abortResult is the payload of a chat.abort response.
type abortResult struct {
	OK      bool     `json:"ok"`
	Aborted bool     `json:"aborted"`
	RunIDs  []string `json:"runIds"`
}

// chatAbort sends a chat.abort and returns its response's payload. The
// response must be the next frame: no event comes before it.
func chatAbort(t *testing.T, em *emulator, conn *websocket.Conn, id string, params map[string]any) abortResult {
	t.Helper()
	em.writeReq(t, conn, id, "chat.abort", params)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var r resFrame
	if err := conn.ReadJSON(&r); err != nil {
		t.Fatalf("reading response %s: %v", id, err)
	}
	if r.Type != gatewayproto.FrameRes || r.ID != id {
		t.Fatalf("frame before the chat.abort response: %+v %s", r, r.Payload)
	}
	if !r.OK {
		t.Fatalf("chat.abort %s refused: %+v", id, r.Error)
	}
	var res abortResult
	if err := json.Unmarshal(r.Payload, &res); err != nil {
		t.Fatalf("decode chat.abort result: %v", err)
	}
	if !res.OK || res.RunIDs == nil {
		t.Fatalf("chat.abort %s result = %s, want ok and a runIds list", id, r.Payload)
	}
	return res
}

// sendRuns sends one chat.send per run id to sessionKey and waits for their
// acks and for the agent layer to have them.
func sendRuns(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn, sessionKey string, ids ...string) {
	t.Helper()
	before := len(a.calls(0))
	for _, id := range ids {
		em.writeReq(t, conn, "send-"+id, "chat.send", map[string]any{"message": "work " + id, "sessionKey": sessionKey, "idempotencyKey": id})
		if r := readRes(t, conn, "send-"+id); !r.OK {
			t.Fatalf("chat.send %s refused: %+v", id, r.Error)
		}
	}
	if got := a.calls(before + len(ids)); len(got) != before+len(ids) {
		t.Fatalf("agent layer got %d messages, want %d", len(got), before+len(ids))
	}
}

// noMoreFrames fails when the connection carries another frame soon.
func noMoreFrames(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var f collectedFrame
	if err := conn.ReadJSON(&f); err == nil {
		t.Fatalf("unexpected frame: %+v %s", f, f.Payload)
	}
}

// chat.abort for the running run stops it the way a /cancel sent from the
// device does: the agent gets one /cancel in the device's chat and session,
// the run ends once as aborted under its own id, and the /cancel's one reply
// follows under a run of its own. Aborting it again is a no-op.
func TestChatAbort_StopsTheRunningTurn(t *testing.T) {
	a, wsURL := newAbortAgent(t)
	em := newEmulator(t)
	conn := mustOpen(t, em, wsURL, "")
	sendRuns(t, a, em, conn, "agent:bob:main", "run-1")

	res := chatAbort(t, em, conn, "ab1", map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"})
	if !res.Aborted || !slices.Equal(res.RunIDs, []string{"run-1"}) {
		t.Fatalf("chat.abort result = %+v, want aborted [run-1]", res)
	}
	calls := a.calls(2)
	if len(calls) != 2 {
		t.Fatalf("agent layer got %+v, want the message and one /cancel", calls)
	}
	cancel := calls[1]
	if cancel.content != "/cancel" || cancel.chatID != "device:"+em.deviceID() || cancel.sessionKey != "agent:bob:main" ||
		cancel.runID == "" || cancel.runID == "run-1" {
		t.Fatalf("cancel handed to the agent = %+v, want /cancel in the device chat and bob's session under a run of its own", cancel)
	}
	if got := a.stoppedRuns(); !slices.Equal(got, []string{"run-1"}) {
		t.Fatalf("stopped turns = %v, want [run-1]", got)
	}

	got := readRunFrames(t, conn, 5)
	want := []runFrame{
		{"run-1", "agent:lifecycle:end"},
		{"run-1", "chat:aborted"},
		{cancel.runID, "agent:assistant"},
		{cancel.runID, "agent:lifecycle:end"},
		{cancel.runID, "chat:final"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}

	if res := chatAbort(t, em, conn, "ab2", map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"}); res.Aborted || len(res.RunIDs) != 0 {
		t.Fatalf("second chat.abort = %+v, want a no-op", res)
	}
	if calls := a.settled(); len(calls) != 2 {
		t.Fatalf("a no-op abort reached the agent: %+v", calls)
	}
	noMoreFrames(t, conn)
}

// Without a run id, chat.abort stops the session's runs of this connection;
// with no session key, it uses the connection's session, as chat.send does.
func TestChatAbort_WithoutRunID(t *testing.T) {
	a, wsURL := newAbortAgent(t)
	em := newEmulator(t)
	conn := mustOpen(t, em, wsURL, "")
	sendRuns(t, a, em, conn, "agent:bob:main", "run-1", "run-2")

	res := chatAbort(t, em, conn, "ab1", map[string]any{})
	if !res.Aborted || !slices.Equal(res.RunIDs, []string{"run-1", "run-2"}) {
		t.Fatalf("chat.abort result = %+v, want aborted [run-1 run-2]", res)
	}
	calls := a.calls(3)
	if len(calls) != 3 || calls[2].content != "/cancel" || calls[2].sessionKey != "agent:bob:main" {
		t.Fatalf("agent layer got %+v, want one /cancel in bob's session", calls)
	}
	got := readRunFrames(t, conn, 7)
	if got[1] != (runFrame{"run-1", "chat:aborted"}) || got[3] != (runFrame{"run-2", "chat:aborted"}) ||
		got[6] != (runFrame{calls[2].runID, "chat:final"}) {
		t.Fatalf("frames = %v", got)
	}
	noMoreFrames(t, conn)
}

// chat.abort with nothing of this connection's to stop in that session
// succeeds with aborted false and sends nothing to the agent: nothing
// running, an unknown or ended run, a run of another session, a run of
// another connection, a run already being aborted, and a device command's
// run.
func TestChatAbort_NoOps(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn)
		abort map[string]any
		// calls is how many messages the agent layer has after the abort.
		calls int
	}{
		{
			name:  "nothing running",
			abort: map[string]any{"sessionKey": "agent:bob:main"},
		},
		{
			name: "unknown run",
			setup: func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				sendRuns(t, a, em, conn, "agent:bob:main", "run-1")
			},
			abort: map[string]any{"sessionKey": "agent:bob:main", "runId": "run-x"},
			calls: 1,
		},
		{
			name: "ended run",
			setup: func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				sendRuns(t, a, em, conn, "agent:bob:main", "run-1")
				if err := a.srv.DeliverReply(t.Context(), "device:"+em.deviceID(), "run-1", "done", nil, ""); err != nil {
					t.Fatal(err)
				}
				readRunFrames(t, conn, 3)
			},
			abort: map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"},
			calls: 1,
		},
		{
			name: "run of another session",
			setup: func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				sendRuns(t, a, em, conn, "agent:alice:main", "run-1")
			},
			abort: map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"},
			calls: 1,
		},
		{
			name: "another session without run id",
			setup: func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				sendRuns(t, a, em, conn, "agent:alice:main", "run-1")
			},
			abort: map[string]any{"sessionKey": "agent:bob:main"},
			calls: 1,
		},
		{
			name: "run of another connection",
			setup: func(t *testing.T, a *abortAgent, _ *emulator, _ *websocket.Conn) {
				t.Helper()
				other := newEmulator(t)
				otherConn := mustOpen(t, other, a.url, "")
				sendRuns(t, a, other, otherConn, "agent:bob:main", "run-1")
			},
			abort: map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"},
			calls: 1,
		},
		{
			name: "run already being aborted",
			setup: func(t *testing.T, a *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				sendRuns(t, a, em, conn, "agent:bob:main", "run-1")
				a.srv.SetInbound(func(string, string, string, string, string, []InboundAttachment) {}) // the /cancel is never answered
				chatAbort(t, em, conn, "first", map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"})
			},
			abort: map[string]any{"sessionKey": "agent:bob:main", "runId": "run-1"},
			calls: 1,
		},
		{
			name: "device command",
			setup: func(t *testing.T, _ *abortAgent, em *emulator, conn *websocket.Conn) {
				t.Helper()
				em.writeReq(t, conn, "help", "chat.send", map[string]any{"message": "/help", "idempotencyKey": "run-help"})
				readRes(t, conn, "help")
				readRunFrames(t, conn, 3)
			},
			abort: map[string]any{"runId": "run-help"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, wsURL := newAbortAgent(t)
			em := newEmulator(t)
			conn := mustOpen(t, em, wsURL, "")
			if tc.setup != nil {
				tc.setup(t, a, em, conn)
			}
			res := chatAbort(t, em, conn, "ab", tc.abort)
			if res.Aborted || len(res.RunIDs) != 0 {
				t.Fatalf("chat.abort = %+v, want aborted false and no runs", res)
			}
			if calls := a.settled(); len(calls) != tc.calls {
				t.Fatalf("agent layer got %+v, want %d messages", calls, tc.calls)
			}
			if got := a.stoppedRuns(); len(got) != 0 {
				t.Fatalf("stopped %v", got)
			}
			noMoreFrames(t, conn)
		})
	}
}

// A chat.abort for a session the device may not reach is refused as its
// chat.send would be, and reaches no agent.
func TestChatAbort_OutOfScopeRefused(t *testing.T) {
	a, wsURL := newAbortAgent(t)
	em := newEmulator(t)
	conn := mustOpen(t, em, wsURL, "")
	if err := a.srv.store.SetDeviceAllowedAgents(t.Context(), em.deviceID(), []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	sendRuns(t, a, em, conn, "agent:bob:main", "run-1")
	for _, tc := range []struct{ key, msg string }{
		{"agent:alice:main", "agent not allowed for this device"},
		{"agent:nobody:main", errUnknownAgent.Error()},
	} {
		for _, method := range []string{"chat.send", "chat.abort"} {
			id := fmt.Sprintf("%s-%s", method, tc.key)
			em.writeReq(t, conn, id, method, map[string]any{"sessionKey": tc.key, "runId": "run-1", "message": "hi", "idempotencyKey": "r2"})
			wantRefused(t, readRes(t, conn, id), id, gatewayproto.CodeInvalidRequest, tc.msg)
		}
	}
	if calls := a.settled(); len(calls) != 1 {
		t.Fatalf("agent layer got %+v, want only the first message", calls)
	}
	noMoreFrames(t, conn)
}

// Malformed chat.abort parameters are refused.
func TestChatAbort_InvalidParams(t *testing.T) {
	_, wsURL := newAbortAgent(t)
	em := newEmulator(t)
	conn := mustOpen(t, em, wsURL, "")
	em.writeReq(t, conn, "ab", "chat.abort", "not an object")
	wantRefused(t, readRes(t, conn, "ab"), "ab", gatewayproto.CodeInvalidRequest, "invalid chat.abort params")
}

// hello-ok advertises chat.abort.
func TestChatAbort_Advertised(t *testing.T) {
	_, wsURL := newAbortAgent(t)
	em := newEmulator(t)
	conn, resp := em.open(t, wsURL, "")
	t.Cleanup(func() { utils.CloseQuietly(conn) })
	if !resp.OK {
		t.Fatalf("handshake failed: %+v", resp.Error)
	}
	if hello := helloOf(t, resp); !slices.Contains(hello.Features.Methods, "chat.abort") {
		t.Fatalf("methods = %v, want chat.abort", hello.Features.Methods)
	}
}
