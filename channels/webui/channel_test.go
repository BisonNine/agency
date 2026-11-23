package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/utils"
)

const testToken = "webui-token-abcdefghijkl"

// liveChannel starts a WebUI channel for cfg behind an httptest server and
// returns it with its bus and the socket URL (without query).
func liveChannel(t *testing.T, cfg config.WebUIConfig) (*WebUIChannel, *bus.MessageBus, string) {
	t.Helper()
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)
	c, err := NewWebUIChannel(cfg, mb)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c)
	t.Cleanup(func() {
		if err := c.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
		srv.Close()
	})
	return c, mb, "ws" + strings.TrimPrefix(srv.URL, "http") + "/webui/ws"
}

// dialToken opens a socket authenticated with the channel token. The handshake
// response is returned so refusals can be inspected; conn is nil on failure.
func dialToken(t *testing.T, wsURL string) (*websocket.Conn, int, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + testToken}})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("close dial response body: %v", closeErr)
		}
	}
	if err == nil {
		t.Cleanup(func() { utils.CloseQuietly(conn) })
	}
	return conn, status, err
}

// mustDial opens a token-authenticated socket or fails the test.
func mustDial(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	conn, _, err := dialToken(t, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// readMsg reads the next protocol message from the client side of a socket.
func readMsg(t *testing.T, conn *websocket.Conn) WebUIMessage {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var m WebUIMessage
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// roundTrip sends a ping and waits for the matching pong, which proves the
// server's read loop for this socket is running.
func roundTrip(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	if err := conn.WriteJSON(WebUIMessage{Type: TypePing, ID: id}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if m := readMsg(t, conn); m.Type != TypePong || m.ID != id {
		t.Fatalf("got %+v, want pong %q", m, id)
	}
}

func consumeInbound(t *testing.T, mb *bus.MessageBus, wait time.Duration) (bus.InboundMessage, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return mb.ConsumeInbound(ctx)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The registered factory builds the channel from channels.webui when the
// manager plans an enabled WebUI.
func TestFactoryRegistered(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.WebUI = config.WebUIConfig{Enabled: true, Token: testToken, AllowFrom: []string{"*"}}
	mb := bus.NewMessageBus()
	defer mb.Close()
	m, err := channels.NewManager(cfg, mb, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ch, ok := m.GetChannel("webui")
	if !ok {
		t.Fatal("webui channel not built")
	}
	wc, ok := ch.(*WebUIChannel)
	if !ok {
		t.Fatalf("built %T", ch)
	}
	if wc.config.Token != testToken {
		t.Fatalf("channel built from the wrong config: %+v", wc.config)
	}
}

func TestNewWebUIChannel_RequiresToken(t *testing.T) {
	if _, err := NewWebUIChannel(config.WebUIConfig{}, nil); err == nil || err.Error() != "webui token is required" {
		t.Fatalf("err = %v", err)
	}
}

func TestServeHTTP_Routing(t *testing.T) {
	c := testChannel(t, testToken)
	if got := c.WebhookPath(); got != "/webui/" {
		t.Fatalf("WebhookPath = %q", got)
	}

	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webui/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status %d, want 404", rec.Code)
	}

	// Both socket paths reach the handler, which refuses while stopped.
	for _, p := range []string{"/webui/ws", "/webui/ws/"} {
		rec = httptest.NewRecorder()
		c.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "channel not running") {
			t.Fatalf("%s while stopped: %d %q", p, rec.Code, rec.Body.String())
		}
	}
}

// An authenticated request that is not a WebSocket handshake is refused by the
// upgrader and leaves no connection behind.
func TestHandleWebSocket_UpgradeFailure(t *testing.T) {
	c := testChannel(t, testToken)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	r := httptest.NewRequest(http.MethodGet, "/webui/ws", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if n := c.connCount.Load(); n != 0 {
		t.Fatalf("connCount = %d after failed upgrade", n)
	}
}

func TestHandleWebSocket_ConnectionLimit(t *testing.T) {
	c, _, wsURL := liveChannel(t, config.WebUIConfig{MaxConnections: 1})

	first := mustDial(t, wsURL)
	roundTrip(t, first, "p1")

	if _, status, err := dialToken(t, wsURL); err == nil || status != http.StatusServiceUnavailable {
		t.Fatalf("second connection: err=%v status=%d, want 503", err, status)
	}

	// Closing the first frees the slot.
	if err := first.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatalf("close frame: %v", err)
	}
	waitUntil(t, "slot freed", func() bool { return c.connCount.Load() == 0 })
	roundTrip(t, mustDial(t, wsURL), "p2")
}

func TestReadLoop_ProtocolErrors(t *testing.T) {
	_, mb, wsURL := liveChannel(t, config.WebUIConfig{AllowFrom: []string{"*"}})
	conn := mustDial(t, wsURL)

	tests := []struct {
		name    string
		raw     string
		code    string
		message string
	}{
		{"not json", "{nope", "invalid_message", "failed to parse message"},
		{"unknown type", `{"type":"bogus"}`, "unknown_type", "unknown message type: bogus"},
		{"missing content", `{"type":"message.send","payload":{}}`, "empty_content", "message content is empty"},
		{"blank content", `{"type":"message.send","payload":{"content":"  "}}`, "empty_content", "message content is empty"},
		{"non-string content", `{"type":"message.send","payload":{"content":7}}`, "empty_content", "message content is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(tt.raw)); err != nil {
				t.Fatalf("write: %v", err)
			}
			m := readMsg(t, conn)
			if m.Type != TypeError || m.Payload["code"] != tt.code || m.Payload["message"] != tt.message {
				t.Fatalf("got %+v, want error %s/%q", m, tt.code, tt.message)
			}
		})
	}
	// The socket survives protocol errors, and none of them reached the bus.
	roundTrip(t, conn, "after")
	if in, ok := consumeInbound(t, mb, 50*time.Millisecond); ok {
		t.Fatalf("rejected message published: %+v", in)
	}
}

// A socket opened without session_id gets a generated session; a message may
// name another session explicitly, which then addresses the reply.
func TestHandleMessageSend_Sessions(t *testing.T) {
	_, mb, wsURL := liveChannel(t, config.WebUIConfig{AllowFrom: []string{"*"}})
	conn := mustDial(t, wsURL)

	if err := conn.WriteJSON(WebUIMessage{Type: TypeMessageSend, ID: "m1", Payload: map[string]any{"content": "first"}}); err != nil {
		t.Fatal(err)
	}
	in, ok := consumeInbound(t, mb, 3*time.Second)
	if !ok {
		t.Fatal("no inbound message")
	}
	generated := strings.TrimPrefix(in.ChatID, "webui:")
	if generated == in.ChatID || generated == "" {
		t.Fatalf("ChatID = %q, want webui:<generated session>", in.ChatID)
	}
	if in.Metadata["platform"] != "webui" || in.Metadata["session_id"] != generated || in.Metadata["conn_id"] == "" {
		t.Fatalf("metadata = %v", in.Metadata)
	}
	if in.Peer.Kind != "direct" || in.Peer.ID != in.ChatID || in.Content != "first" {
		t.Fatalf("inbound = %+v", in)
	}

	if err := conn.WriteJSON(WebUIMessage{Type: TypeMessageSend, SessionID: "chosen", Payload: map[string]any{"content": "second"}}); err != nil {
		t.Fatal(err)
	}
	in, ok = consumeInbound(t, mb, 3*time.Second)
	if !ok {
		t.Fatal("no inbound message")
	}
	if in.ChatID != "webui:chosen" || in.Metadata["session_id"] != "chosen" {
		t.Fatalf("explicit session ignored: chat=%q metadata=%v", in.ChatID, in.Metadata)
	}
}

func TestHandleMessageSend_DisallowedSender(t *testing.T) {
	_, mb, wsURL := liveChannel(t, config.WebUIConfig{AllowFrom: []string{"webui:someone-else"}})
	conn := mustDial(t, wsURL)
	if err := conn.WriteJSON(WebUIMessage{Type: TypeMessageSend, Payload: map[string]any{"content": "hi"}}); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn, "sync") // the send has been processed
	if in, ok := consumeInbound(t, mb, 50*time.Millisecond); ok {
		t.Fatalf("disallowed sender published: %+v", in)
	}
}

// Replies, edits, typing and placeholders reach every socket of the session.
func TestOutbound_ToLiveSession(t *testing.T) {
	c, _, wsURL := liveChannel(t, config.WebUIConfig{
		Placeholder: config.PlaceholderConfig{Enabled: true},
	})
	conn := mustDial(t, wsURL+"?session_id=s1")
	roundTrip(t, conn, "ready")
	ctx := context.Background()

	if err := c.Send(ctx, bus.OutboundMessage{ChatID: "webui:s1", Content: "reply"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m := readMsg(t, conn); m.Type != TypeMessageCreate || m.SessionID != "s1" || m.Payload["content"] != "reply" {
		t.Fatalf("send delivered %+v", m)
	}

	if err := c.EditMessage(ctx, "webui:s1", "msg-1", "edited"); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	if m := readMsg(t, conn); m.Type != TypeMessageUpdate || m.Payload["message_id"] != "msg-1" || m.Payload["content"] != "edited" {
		t.Fatalf("edit delivered %+v", m)
	}

	stop, err := c.StartTyping(ctx, "webui:s1")
	if err != nil {
		t.Fatalf("StartTyping: %v", err)
	}
	if m := readMsg(t, conn); m.Type != TypeTypingStart || m.SessionID != "s1" {
		t.Fatalf("typing start delivered %+v", m)
	}
	stop()
	if m := readMsg(t, conn); m.Type != TypeTypingStop || m.SessionID != "s1" {
		t.Fatalf("typing stop delivered %+v", m)
	}

	id, err := c.SendPlaceholder(ctx, "webui:s1")
	if err != nil || id == "" {
		t.Fatalf("SendPlaceholder = %q, %v", id, err)
	}
	if m := readMsg(t, conn); m.Type != TypeMessageCreate || m.Payload["content"] != "Thinking... 💭" || m.Payload["message_id"] != id {
		t.Fatalf("placeholder delivered %+v", m)
	}
}

func TestSendPlaceholder(t *testing.T) {
	t.Run("disabled sends nothing", func(t *testing.T) {
		c := testChannel(t, testToken)
		id, err := c.SendPlaceholder(context.Background(), "webui:nosuch")
		if id != "" || err != nil {
			t.Fatalf("got %q, %v; want no placeholder", id, err)
		}
	})
	t.Run("custom text", func(t *testing.T) {
		c, _, wsURL := liveChannel(t, config.WebUIConfig{
			Placeholder: config.PlaceholderConfig{Enabled: true, Text: "Working on it"},
		})
		conn := mustDial(t, wsURL+"?session_id=s1")
		roundTrip(t, conn, "ready")
		if _, err := c.SendPlaceholder(context.Background(), "webui:s1"); err != nil {
			t.Fatal(err)
		}
		if m := readMsg(t, conn); m.Payload["content"] != "Working on it" {
			t.Fatalf("placeholder %+v", m)
		}
	})
	t.Run("offline session", func(t *testing.T) {
		c, err := NewWebUIChannel(config.WebUIConfig{Token: testToken, Placeholder: config.PlaceholderConfig{Enabled: true}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := c.SendPlaceholder(context.Background(), "webui:nosuch")
		if id != "" || !errors.Is(err, channels.ErrRecipientOffline) {
			t.Fatalf("got %q, %v; want ErrRecipientOffline", id, err)
		}
	})
}

func TestStartTyping_Offline(t *testing.T) {
	c := testChannel(t, testToken)
	stop, err := c.StartTyping(context.Background(), "webui:nosuch")
	if !errors.Is(err, channels.ErrRecipientOffline) {
		t.Fatalf("err = %v, want ErrRecipientOffline", err)
	}
	if stop == nil {
		t.Fatal("stop must be callable even on failure")
	}
	stop()
}

// The tab closing between typing start and stop leaves stop harmless.
func TestStartTyping_StopAfterTabClosed(t *testing.T) {
	c := testChannel(t, testToken)
	pc := addConn(c, "c1", "s1", serverConn(t))
	stop, err := c.StartTyping(context.Background(), "webui:s1")
	if err != nil {
		t.Fatalf("StartTyping: %v", err)
	}
	pc.close()
	stop()
	if err := c.broadcastToSession("webui:s1", newMessage(TypeTypingStop, nil)); !errors.Is(err, channels.ErrRecipientOffline) {
		t.Fatalf("session should be offline after the tab closed, got %v", err)
	}
}

func TestSend_NotRunning(t *testing.T) {
	c := testChannel(t, testToken)
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "webui:s1", Content: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

func TestWriteJSON_Closed(t *testing.T) {
	pc := &webuiConn{id: "c1"}
	pc.closed.Store(true)
	if err := pc.writeJSON(newMessage(TypePong, nil)); !errors.Is(err, errConnClosed) {
		t.Fatalf("err = %v, want errConnClosed", err)
	}
	// send swallows the failure: the read loop notices the closed peer itself.
	pc.send(newMessage(TypePong, nil))
}

// Once the channel's context is cancelled, the read loop closes the socket:
// at once if it was between messages, else after the message in hand (whose
// reply may still arrive).
func TestReadLoop_StopsOnContextCancel(t *testing.T) {
	c, _, wsURL := liveChannel(t, config.WebUIConfig{})
	conn := mustDial(t, wsURL)
	roundTrip(t, conn, "before")

	c.cancel()
	// Wake a read loop blocked in its read. One that already returned has
	// closed the socket, which the read below reports either way.
	if err := conn.WriteJSON(WebUIMessage{Type: TypePing, ID: "in-hand"}); err != nil {
		t.Logf("write after cancel: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for replies := 0; ; replies++ {
		var m WebUIMessage
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		if replies > 0 || m.Type != TypePong || m.ID != "in-hand" {
			t.Fatalf("socket still open after the context was cancelled: got %+v", m)
		}
	}
	waitUntil(t, "connection released", func() bool { return c.connCount.Load() == 0 })
}

// Stop closes every open socket and forgets it.
func TestStop_ClosesOpenSockets(t *testing.T) {
	c, _, wsURL := liveChannel(t, config.WebUIConfig{})
	conn := mustDial(t, wsURL)
	roundTrip(t, conn, "ready")

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("socket still open after Stop")
	}
	registered := false
	c.connections.Range(func(any, any) bool { registered = true; return false })
	if registered {
		t.Fatal("connection still registered after Stop")
	}
}

// A client closing with an unusual close code is logged and released.
func TestReadLoop_UnexpectedCloseReleasesConnection(t *testing.T) {
	c, _, wsURL := liveChannel(t, config.WebUIConfig{})
	conn := mustDial(t, wsURL)
	roundTrip(t, conn, "ready")
	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "app error")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "connection released", func() bool { return c.connCount.Load() == 0 })
	empty := true
	c.connections.Range(func(any, any) bool { empty = false; return false })
	if !empty {
		t.Fatal("closed connection still registered")
	}
}

// The server pings at the configured interval, and each pong extends the read
// deadline so an idle but responsive browser stays connected past ReadTimeout.
func TestPingKeepsIdleConnectionAlive(t *testing.T) {
	_, _, wsURL := liveChannel(t, config.WebUIConfig{PingInterval: 1, ReadTimeout: 2})
	conn := mustDial(t, wsURL)

	var pings atomic.Int32
	conn.SetPingHandler(func(data string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	// Control frames are only processed while the client reads.
	got := make(chan WebUIMessage, 1)
	go func() {
		var m WebUIMessage
		if err := conn.ReadJSON(&m); err == nil {
			got <- m
		}
		close(got)
	}()

	// Three pings take us past the 2s ReadTimeout with no data frame sent.
	waitUntil(t, "three pings", func() bool { return pings.Load() >= 3 })
	if err := conn.WriteJSON(WebUIMessage{Type: TypePing, ID: "alive"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case m, ok := <-got:
		if !ok || m.Type != TypePong || m.ID != "alive" {
			t.Fatalf("connection did not survive idle period: %+v ok=%v", m, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pong")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 50); got != "short" {
		t.Fatalf("short: %q", got)
	}
	if got := truncate("héllo wörld", 5); got != "héllo..." {
		t.Fatalf("runes: %q", got)
	}
}
