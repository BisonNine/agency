package device

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/gatewayproto"
	"go.bison9.com/agency/utils"
)

// wsPair returns the server side and the client side of a live loopback
// websocket connection, both closed when the test ends.
func wsPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	got := make(chan *websocket.Conn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		got <- c
	}))
	t.Cleanup(hs.Close)
	client, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close dial response body: %v", cerr)
		}
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-got
	t.Cleanup(func() {
		utils.CloseQuietly(client)
		utils.CloseQuietly(server)
	})
	return server, client
}

// testLiveConn is a post-handshake connection for deviceID whose writer is
// not running: what the server sends stays in its queue (see queued).
func testLiveConn(t *testing.T, deviceID string) *liveConn {
	t.Helper()
	conn, _ := wsPair(t)
	return &liveConn{cw: newConnWriter(conn), deviceID: deviceID, chatID: "device:" + deviceID}
}

// queued decodes the next frame the server queued on lc.
func queued(t *testing.T, lc *liveConn) resFrame {
	t.Helper()
	select {
	case v := <-lc.cw.queue:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal queued frame: %v", err)
		}
		var f resFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decode queued frame: %v", err)
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("nothing queued")
		return resFrame{}
	}
}

// queuedReplyText is the text of the chat final event among the next frames
// the server queued on lc for one reply (agent text, lifecycle end, chat).
func queuedReplyText(t *testing.T, lc *liveConn) string {
	t.Helper()
	for range 3 {
		f := queued(t, lc)
		if f.Event != "chat" {
			continue
		}
		var ce struct {
			State   string `json:"state"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		if err := json.Unmarshal(f.Payload, &ce); err != nil {
			t.Fatal(err)
		}
		return ce.Message.Text
	}
	t.Fatal("no chat final among the queued frames")
	return ""
}

func wantRefused(t *testing.T, f resFrame, id string, code gatewayproto.ErrorCode, msg string) {
	t.Helper()
	if f.ID != id || f.OK || f.Error == nil || f.Error.Code != code || !strings.Contains(f.Error.Message, msg) {
		t.Fatalf("response = %+v (error %+v), want %s refused %s %q", f, f.Error, id, code, msg)
	}
}

func req(id, method, params string) gatewayproto.RequestFrame {
	return gatewayproto.RequestFrame{Type: gatewayproto.FrameReq, ID: id, Method: method, Params: json.RawMessage(params)}
}

// The origin allowlist admits listed origins and "*", and refuses others;
// an empty list admits every origin.
func TestCheckOrigin(t *testing.T) {
	check := func(allow []string, origin string) bool {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Origin", origin)
		return NewServer(nil, ServerOptions{AllowOrigins: allow}).upgrader.CheckOrigin(r)
	}
	for _, tc := range []struct {
		allow  []string
		origin string
		want   bool
	}{
		{nil, "https://evil.example", true},
		{[]string{"https://ok.example"}, "https://ok.example", true},
		{[]string{"https://ok.example"}, "https://evil.example", false},
		{[]string{"https://ok.example", "*"}, "https://evil.example", true},
	} {
		if got := check(tc.allow, tc.origin); got != tc.want {
			t.Errorf("allow %v, origin %q: %v, want %v", tc.allow, tc.origin, got, tc.want)
		}
	}
}

// A connection from an origin outside the allowlist is refused at upgrade.
func TestHandleWS_ForbiddenOrigin(t *testing.T) {
	_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AllowOrigins: []string{"https://ok.example"}})
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{"https://evil.example"}})
	if resp != nil && resp.Body != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close dial response body: %v", cerr)
		}
	}
	if err == nil {
		utils.CloseQuietly(conn)
		t.Fatal("dial from a forbidden origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %+v, want 403", resp)
	}
}

// connectOpts shapes one handshake for rawConnect.
type connectOpts struct {
	role       string                    // connect role ("" = none)
	auth       *gatewayproto.ConnectAuth // presented auth
	signToken  string                    // the token the signature covers
	publicKey  string                    // overrides the encoded key when set
	badSig     bool                      // sign a different payload
	wantClosed bool                      // expect no response, only a close
}

// rawConnect runs a handshake for em shaped by o and returns the connection
// (closed at test end) and the response. With o.wantClosed it returns the
// close code instead of a response.
func rawConnect(t *testing.T, em *emulator, wsURL string, o connectOpts) (*websocket.Conn, resFrame, int) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close dial response body: %v", cerr)
		}
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { utils.CloseQuietly(conn) })
	var ch struct {
		Payload gatewayproto.ChallengePayload `json:"payload"`
	}
	if err := conn.ReadJSON(&ch); err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	signedAt := time.Now().UnixMilli()
	b64 := base64.RawURLEncoding.EncodeToString
	in := gatewayproto.DeviceAuthInput{
		DeviceID: em.deviceID(), ClientID: "rabbit-r1", ClientMode: gatewayproto.ModeNode,
		Role: gatewayproto.RoleNode, SignedAtMs: signedAt, Token: o.signToken,
		Nonce: ch.Payload.Nonce, Platform: "rabbit", DeviceFamily: "r1",
	}
	payload := gatewayproto.BuildDeviceAuthPayloadV3(in)
	if o.badSig {
		payload += "tampered"
	}
	pub := b64(em.pub)
	if o.publicKey != "" {
		pub = o.publicKey
	}
	params := gatewayproto.ConnectParams{
		MinProtocol: 4, MaxProtocol: 4,
		Client: gatewayproto.ClientInfo{ID: "rabbit-r1", Version: "1.0", Platform: "rabbit", DeviceFamily: "r1", Mode: gatewayproto.ModeNode},
		Role:   o.role,
		Auth:   o.auth,
		Device: &gatewayproto.DeviceIdentity{ID: em.deviceID(), PublicKey: pub, Signature: b64(ed25519.Sign(em.priv, []byte(payload))), SignedAt: signedAt, Nonce: ch.Payload.Nonce},
	}
	em.writeReq(t, conn, "c1", "connect", params)
	if o.wantClosed {
		return conn, resFrame{}, readUntilClose(t, conn, 5*time.Second)
	}
	return conn, readRes(t, conn, "c1"), 0
}

func helloFrom(t *testing.T, f resFrame) gatewayproto.HelloOk {
	t.Helper()
	if !f.OK {
		t.Fatalf("connect refused: %+v", f.Error)
	}
	var h gatewayproto.HelloOk
	if err := json.Unmarshal(f.Payload, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// A connect with no role is a node; a device may authenticate with its device
// token or (with open auth) a bootstrap token, signing over that token.
func TestHandshake_RoleAndTokenFields(t *testing.T) {
	t.Run("no role", func(t *testing.T) {
		_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
		_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{})
		if h := helloFrom(t, f); h.Auth.Role != gatewayproto.RoleNode {
			t.Fatalf("role = %q, want node", h.Auth.Role)
		}
	})
	t.Run("device token", func(t *testing.T) {
		_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true, SharedToken: "shared"})
		em := newEmulator(t)
		tok := helloOf(t, em.connect(t, wsURL, "shared")).Auth.DeviceToken
		_, f, _ := rawConnect(t, em, wsURL, connectOpts{role: gatewayproto.RoleNode, auth: &gatewayproto.ConnectAuth{DeviceToken: tok}, signToken: tok})
		if h := helloFrom(t, f); h.Auth.DeviceToken != tok {
			t.Fatalf("device token = %q, want the presented one echoed", h.Auth.DeviceToken)
		}
	})
	t.Run("bootstrap token", func(t *testing.T) {
		_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
		_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode, auth: &gatewayproto.ConnectAuth{BootstrapToken: "boot"}, signToken: "boot"})
		helloFrom(t, f)
	})
}

// A device key that does not decode, or a signature that does not verify,
// is refused.
func TestHandshake_DeviceIdentityRefusals(t *testing.T) {
	_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode, publicKey: "!!not-base64!!"})
	wantRefused(t, f, "c1", gatewayproto.CodeInvalidRequest, "invalid device public key")
	_, f, _ = rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode, badSig: true})
	wantRefused(t, f, "c1", gatewayproto.CodeInvalidRequest, "device signature verification failed")
}

// With message logging on, a failed authentication is still refused.
func TestHandshake_AuthFailureLogged(t *testing.T) {
	_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", SharedToken: "shared", LogMessages: true})
	_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode, auth: &gatewayproto.ConnectAuth{Token: "wrong"}, signToken: "wrong"})
	wantRefused(t, f, "c1", gatewayproto.CodeInvalidRequest, "authentication failed")
}

// A pairing store that cannot be read or written refuses the handshake as
// unavailable (close 1011), never as "not paired".
func TestHandshake_StoreFailures(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		_, store, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
		utils.CloseQuietly(store)
		_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode})
		wantRefused(t, f, "c1", gatewayproto.CodeUnavailable, "pairing store error")
	})
	t.Run("create pending", func(t *testing.T) {
		_, store, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1"})
		failOn(t, store, "INSERT", "pending_pairings")
		conn, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode})
		wantRefused(t, f, "c1", gatewayproto.CodeUnavailable, "pairing store error")
		if code := readUntilClose(t, conn, 5*time.Second); code != websocket.CloseInternalServerErr {
			t.Fatalf("close code = %d, want %d", code, websocket.CloseInternalServerErr)
		}
	})
}

// When auto-approve fails the pairing stays pending and the device is told
// pairing is required.
func TestHandshake_AutoApproveFails(t *testing.T) {
	_, store, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	failOn(t, store, "INSERT", "paired_devices")
	_, f, _ := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode})
	wantRefused(t, f, "c1", gatewayproto.CodeNotPaired, "device pairing required")
	if pend, err := store.ListPending(context.Background()); err != nil || len(pend) != 1 {
		t.Fatalf("pending = %+v, %v; want the request kept", pend, err)
	}
}

// A last-seen stamp that cannot be written does not refuse a paired device;
// tokens that cannot be issued leave hello-ok without one.
func TestHandshake_BestEffortWrites(t *testing.T) {
	t.Run("last seen", func(t *testing.T) {
		_, store, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
		em := newEmulator(t)
		helloOf(t, em.connect(t, wsURL, ""))
		failOn(t, store, "UPDATE", "paired_devices")
		_, f, _ := rawConnect(t, em, wsURL, connectOpts{role: gatewayproto.RoleNode})
		helloFrom(t, f)
	})
	t.Run("issue tokens", func(t *testing.T) {
		_, store, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
		em := newEmulator(t)
		helloOf(t, em.connect(t, wsURL, ""))
		failOn(t, store, "INSERT", "device_tokens")
		_, f, _ := rawConnect(t, em, wsURL, connectOpts{role: gatewayproto.RoleNode})
		if h := helloFrom(t, f); h.Auth.DeviceToken != "" || len(h.Auth.DeviceTokens) != 0 {
			t.Fatalf("hello-ok auth = %+v, want no tokens", h.Auth)
		}
	})
}

// A device that completes its handshake after the server stopped is closed
// with "going away" before hello-ok.
func TestHandshake_AfterCloseAll(t *testing.T) {
	srv, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	srv.CloseAll()
	_, _, code := rawConnect(t, newEmulator(t), wsURL, connectOpts{role: gatewayproto.RoleNode, wantClosed: true})
	if code != websocket.CloseGoingAway {
		t.Fatalf("close code = %d, want %d", code, websocket.CloseGoingAway)
	}
}

// The serve loop skips frames that are not requests and takes a pong as
// liveness, then keeps answering.
func TestServeLoop_IgnoresNonRequestsAndPongs(t *testing.T) {
	_, _, wsURL := newTestServer(t, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	em := newEmulator(t)
	conn := mustOpen(t, em, wsURL, "")
	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "event", "event": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	em.writeReq(t, conn, "u1", "no.such.method", map[string]any{})
	wantRefused(t, readRes(t, conn, "u1"), "u1", gatewayproto.CodeInvalidRequest, "method not supported: no.such.method")
	em.writeReq(t, conn, "h1", "health", map[string]any{})
	if r := readRes(t, conn, "h1"); !r.OK {
		t.Fatalf("health after skipped frames: %+v", r.Error)
	}
}

// Malformed params of each request are refused as invalid, naming the method.
func TestHandlers_InvalidParams(t *testing.T) {
	s := newScopeServer(t)
	lc := testLiveConn(t, "dev1")
	ctx := context.Background()

	s.handleChatSend(ctx, lc, req("m1", "chat.send", `"text"`))
	wantRefused(t, queued(t, lc), "m1", gatewayproto.CodeInvalidRequest, "invalid chat.send params")
	s.handleNodeEvent(lc, req("n1", "node.event", `[]`))
	wantRefused(t, queued(t, lc), "n1", gatewayproto.CodeInvalidRequest, "invalid node.event params")
	s.handleChatHistory(ctx, lc, req("h1", "chat.history", `"text"`))
	wantRefused(t, queued(t, lc), "h1", gatewayproto.CodeInvalidRequest, "invalid chat.history params")
}

// Without an agent querier, agents.list and chat.history are not supported.
func TestHandlers_NoQuerier(t *testing.T) {
	s := newScopeServer(t)
	s.querier = nil
	lc := testLiveConn(t, "dev1")
	s.handleAgentsList(context.Background(), lc, req("a1", "agents.list", `{}`))
	wantRefused(t, queued(t, lc), "a1", gatewayproto.CodeInvalidRequest, "method not supported: agents.list")
	s.handleChatHistory(context.Background(), lc, req("h1", "chat.history", `{}`))
	wantRefused(t, queued(t, lc), "h1", gatewayproto.CodeInvalidRequest, "method not supported: chat.history")
}

// A device record that cannot be read refuses agents.list as unavailable.
func TestAgentsList_RecordUnavailable(t *testing.T) {
	s := newScopeServer(t)
	utils.CloseQuietly(s.store)
	lc := testLiveConn(t, "dev1")
	s.handleAgentsList(context.Background(), lc, req("a1", "agents.list", `{}`))
	wantRefused(t, queued(t, lc), "a1", gatewayproto.CodeUnavailable, errDeviceRecordUnavailable.Error())
}

// namedQuerier is stubQuerier whose agents have display names and whose
// default agent may be outside the list.
type namedQuerier struct {
	stubQuerier
	historyKey *string
}

func (q namedQuerier) Agents() ([]DeviceAgentInfo, string, string) {
	return []DeviceAgentInfo{{ID: "alice", Name: "Alice"}, {ID: "bob"}}, q.defaultAgent, "agent:" + q.defaultAgent + ":main"
}

func (q namedQuerier) History(key string) []DeviceHistoryMessage {
	if q.historyKey != nil {
		*q.historyKey = key
	}
	return nil
}

// agents.list carries an agent's display name when it has one.
func TestAgentsList_Names(t *testing.T) {
	s := newScopeServer(t)
	s.querier = namedQuerier{defaultAgent: "alice"}
	lc := testLiveConn(t, "dev1")
	s.handleAgentsList(context.Background(), lc, req("a1", "agents.list", `{}`))
	f := queued(t, lc)
	var p struct {
		Agents []map[string]string `json:"agents"`
	}
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Agents) != 2 || p.Agents[0]["name"] != "Alice" || p.Agents[1]["id"] != "bob" || p.Agents[1]["name"] != "" {
		t.Fatalf("agents = %v, want alice named Alice and bob unnamed", p.Agents)
	}
}

// chat.history with no session key reads the connection's session.
func TestChatHistory_ConnectionSession(t *testing.T) {
	s := newScopeServer(t)
	var read string
	s.querier = namedQuerier{defaultAgent: "alice", historyKey: &read}
	lc := testLiveConn(t, "dev1")
	lc.sessionKey = "agent:bob:main"
	s.handleChatHistory(context.Background(), lc, req("h1", "chat.history", `{}`))
	if f := queued(t, lc); !f.OK {
		t.Fatalf("chat.history refused: %+v", f.Error)
	}
	if read != "agent:bob:main" {
		t.Fatalf("history read %q, want the connection's session agent:bob:main", read)
	}
}

// chat.send with no idempotency key gets a generated run id; with no session
// key it uses the connection's; attachments that are not base64 or are too
// large are dropped and the rest reach the agent.
func TestChatSend_DefaultsAndAttachments(t *testing.T) {
	s := newScopeServer(t)
	s.opts.LogMessages = true
	type turn struct {
		key  string
		atts []InboundAttachment
	}
	got := make(chan turn, 1)
	s.inbound = func(_, _, _, _, key string, atts []InboundAttachment) { got <- turn{key, atts} }
	lc := testLiveConn(t, "dev1")
	lc.sessionKey = "agent:bob:main"

	big := base64.StdEncoding.EncodeToString(make([]byte, maxDeviceAttachmentBytes+1))
	params, err := json.Marshal(map[string]any{
		"message": "look",
		"attachments": []map[string]string{
			{"mimeType": "image/png", "data": "!!not base64!!"},
			{"mimeType": "image/png", "data": big},
			{"mimeType": "image/png", "name": "ok.png", "data": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.handleChatSend(context.Background(), lc, req("m1", "chat.send", string(params)))

	f := queued(t, lc)
	var ack struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(f.Payload, &ack); err != nil || !f.OK || ack.RunID == "" {
		t.Fatalf("ack = %+v (%v), want a generated run id", f, err)
	}
	select {
	case tr := <-got:
		if tr.key != "agent:bob:main" {
			t.Errorf("session key = %q, want the connection's agent:bob:main", tr.key)
		}
		if len(tr.atts) != 1 || tr.atts[0].Name != "ok.png" {
			t.Errorf("attachments = %+v, want only ok.png", tr.atts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the agent layer")
	}
}

// agentCommandServer is a scope server with dev1 paired, and dev1's connection.
func agentCommandServer(t *testing.T) (*Server, *liveConn) {
	t.Helper()
	s := newScopeServer(t)
	pairStoreDevice(t, s.store, "dev1")
	return s, testLiveConn(t, "dev1")
}

// /agent answers a device record it cannot read, no configured agents, and
// an assignment it cannot store.
func TestAgentCommand_Failures(t *testing.T) {
	t.Run("record unreadable", func(t *testing.T) {
		s, lc := agentCommandServer(t)
		utils.CloseQuietly(s.store)
		s.handleAgentCommand(context.Background(), lc, "r1", "")
		if got := queuedReplyText(t, lc); got != "This device's settings could not be read." {
			t.Fatalf("reply = %q", got)
		}
	})
	t.Run("no agents", func(t *testing.T) {
		s, lc := agentCommandServer(t)
		s.querier = nil
		s.handleAgentCommand(context.Background(), lc, "r1", "")
		if got := queuedReplyText(t, lc); got != "No assistants are configured." {
			t.Fatalf("reply = %q", got)
		}
	})
	for _, tc := range []struct{ arg, want string }{
		{"default", "Couldn't reset assistant: "},
		{"bob", "Couldn't switch assistant: "},
	} {
		t.Run("store "+tc.arg, func(t *testing.T) {
			s, lc := agentCommandServer(t)
			failOn(t, s.store, "UPDATE", "paired_devices")
			s.handleAgentCommand(context.Background(), lc, "r1", tc.arg)
			if got := queuedReplyText(t, lc); !strings.HasPrefix(got, tc.want) || !strings.Contains(got, "injected") {
				t.Fatalf("reply = %q, want %q and the store error", got, tc.want)
			}
		})
	}
}

// /agent default names the agent the device now reaches by its id when that
// agent is not one the device can list (a default that is not listed).
func TestAgentCommand_ResetToUnlistedDefault(t *testing.T) {
	s, lc := agentCommandServer(t)
	s.querier = namedQuerier{defaultAgent: "person"}
	s.handleAgentCommand(context.Background(), lc, "r1", "default")
	if got := queuedReplyText(t, lc); got != "Switched to person. New messages will go to this assistant." {
		t.Fatalf("reply = %q", got)
	}
}

// Without a querier no agent named by a session key is known, an assignment
// is honoured unchecked, and a restricted device falls back to its first
// allowed agent.
func TestSessionScope_NoQuerier(t *testing.T) {
	ctx := context.Background()
	s := newScopeServer(t)
	s.querier = nil
	pairStoreDevice(t, s.store, "dev1")
	lc := &liveConn{deviceID: "dev1"}

	if _, err := s.sessionScopeKeyFor(ctx, lc, "agent:bob:main"); !errors.Is(err, errUnknownAgent) {
		t.Fatalf("named agent without a querier: err = %v, want errUnknownAgent", err)
	}
	if err := s.store.SetDeviceAllowedAgents(ctx, "dev1", []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.sessionScopeKeyFor(ctx, lc, ""); err != nil || got != "agent:bob:main" {
		t.Fatalf("restricted device = %q, %v; want its first allowed agent bob", got, err)
	}
	if err := s.store.SetDeviceAgent(ctx, "dev1", "bob"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.sessionScopeKeyFor(ctx, lc, ""); err != nil || got != "agent:bob:main" {
		t.Fatalf("assigned device = %q, %v; want its assignment bob", got, err)
	}
}

// StreamDelta drops a delta for a chat with no connection, and for a
// connection whose only open runs are /cancel.
func TestStreamDelta_NothingToCorrelate(t *testing.T) {
	s := newScopeServer(t)
	if s.StreamDelta("device:nobody", "", "hi") {
		t.Fatal("StreamDelta to no connection reported sent")
	}
	lc := testLiveConn(t, "dev1")
	s.conns.Store(lc.chatID, lc)
	lc.startRun("c1", "", true)
	if s.StreamDelta(lc.chatID, "", "hi") {
		t.Fatal("StreamDelta with only a /cancel run open reported sent")
	}
	if s.StreamDelta(lc.chatID, "unknown-run", "hi") {
		t.Fatal("StreamDelta for an unknown run reported sent")
	}
}

// A connection keeps at most maxOpenRuns open runs and remembers at most
// maxEndedRuns ended ones, oldest dropped first.
func TestRunBounds(t *testing.T) {
	lc := &liveConn{}
	for i := range maxOpenRuns + 2 {
		lc.startRun("run"+string(rune('a'+i)), "", false)
	}
	if len(lc.runs) != maxOpenRuns || lc.runs[0].id != "runc" {
		t.Fatalf("open runs = %d (first %q), want %d starting at runc", len(lc.runs), lc.runs[0].id, maxOpenRuns)
	}
	lc.mu.Lock()
	for i := range maxEndedRuns + 3 {
		lc.endedRun("e" + strings.Repeat("x", i))
	}
	lc.mu.Unlock()
	if len(lc.ended) != maxEndedRuns || lc.ended[0] != "e"+strings.Repeat("x", 3) {
		t.Fatalf("ended runs = %d (first %q), want %d with the oldest three dropped", len(lc.ended), lc.ended[0], maxEndedRuns)
	}
}

// A message merged into an errored or cancelled turn ends with that state.
func TestEndState(t *testing.T) {
	for outcome, want := range map[string]string{bus.OutcomeError: "error", bus.OutcomeCancelled: "aborted", "": "final"} {
		if got := endState(outcome); got != want {
			t.Errorf("endState(%q) = %q, want %q", outcome, got, want)
		}
	}
}

// With message logging on, a reply that also ends a merged message still
// sends that message's end and the reply.
func TestDeliverReply_MergedLogged(t *testing.T) {
	s := newScopeServer(t)
	s.opts.LogMessages = true
	lc := testLiveConn(t, "dev1")
	s.conns.Store(lc.chatID, lc)
	lc.startRun("r1", "", false)
	lc.startRun("r2", "", false)
	if err := s.DeliverReply(context.Background(), lc.chatID, "r2", "done", []string{"r1"}, bus.OutcomeError); err != nil {
		t.Fatalf("DeliverReply: %v", err)
	}
	var states []string
	for range 5 {
		f := queued(t, lc)
		if f.Event != "chat" {
			continue
		}
		var ce struct {
			RunID string `json:"runId"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(f.Payload, &ce); err != nil {
			t.Fatal(err)
		}
		states = append(states, ce.RunID+":"+ce.State)
	}
	if strings.Join(states, ",") != "r1:error,r2:final" {
		t.Fatalf("chat events = %v, want r1:error then r2:final", states)
	}
}

func TestClientIP_NilRequest(t *testing.T) {
	if got := clientIP(nil); got != "" {
		t.Fatalf("clientIP(nil) = %q, want empty", got)
	}
}

// A writer whose connection is gone fails its writes and closes; send on a
// full queue closes the connection; a close that cannot be queued closes at
// once.
func TestConnWriter_Failures(t *testing.T) {
	t.Run("full queue", func(t *testing.T) {
		conn, client := wsPair(t)
		w := &connWriter{conn: conn, queue: make(chan any, 1), done: make(chan struct{})}
		if !w.send("a") {
			t.Fatal("first send refused")
		}
		if w.send("b") {
			t.Fatal("send on a full queue reported queued")
		}
		select {
		case <-w.done:
		default:
			t.Fatal("writer not closed after its queue filled")
		}
		if code := readUntilClose(t, client, 5*time.Second); code != websocket.CloseAbnormalClosure {
			t.Fatalf("client saw close code %d, want the connection dropped without a close frame", code)
		}
		// Closed: a close frame cannot be queued, and sendClose still closes.
		w.sendClose(websocket.CloseNormalClosure, "bye")
	})
	t.Run("closed conn", func(t *testing.T) {
		conn, _ := wsPair(t)
		w := newConnWriter(conn)
		utils.CloseQuietly(conn)
		if err := w.writeJSON(map[string]string{"a": "b"}); err == nil {
			t.Fatal("writeJSON on a closed connection succeeded")
		}
		select {
		case <-w.done:
		default:
			t.Fatal("writer not closed after a failed write")
		}
		if err := w.ping(); err == nil {
			t.Fatal("ping on a closed connection succeeded")
		}
		w.closeWith(websocket.CloseNormalClosure, "bye") // logs, does not panic
		setReadDeadline(conn, time.Now())                // logs, does not panic
	})
	t.Run("ping", func(t *testing.T) {
		conn, client := wsPair(t)
		var wg sync.WaitGroup
		pinged := make(chan struct{}, 1)
		client.SetPingHandler(func(string) error { pinged <- struct{}{}; return nil })
		// The read ends with an error when the test closes client.
		wg.Go(func() {
			if _, _, err := client.ReadMessage(); err == nil {
				t.Error("unexpected data frame")
			}
		})
		if err := newConnWriter(conn).ping(); err != nil {
			t.Fatalf("ping: %v", err)
		}
		select {
		case <-pinged:
		case <-time.After(5 * time.Second):
			t.Fatal("client never saw the ping")
		}
		utils.CloseQuietly(client)
		wg.Wait()
	})
}
