package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
)

const (
	stubUser = "@agency:matrix.test"
	stubRoom = "!room:matrix.test"
)

// stubReq is one request the stub homeserver received.
type stubReq struct {
	Method string
	Path   string
	Body   map[string]any
}

// stubHomeserver answers the client calls the channel makes outside /sync and
// records each one. A path containing a key of fail is answered with that
// status. members is the joined-member count of every room; roomName is every
// room's name ("" = none); download serves media downloads.
type stubHomeserver struct {
	t    *testing.T
	url  string
	reqs chan stubReq

	mu       sync.Mutex
	fail     map[string]int
	members  int
	roomName string
	download http.HandlerFunc
}

func newStubHomeserver(t *testing.T) *stubHomeserver {
	t.Helper()
	hs := &stubHomeserver{t: t, reqs: make(chan stubReq, 64), fail: map[string]int{}, members: 2}
	srv := httptest.NewServer(http.HandlerFunc(hs.serve))
	t.Cleanup(srv.Close)
	hs.url = srv.URL
	return hs
}

func (hs *stubHomeserver) setFail(pathPart string, status int) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.fail[pathPart] = status
}

func (hs *stubHomeserver) serve(w http.ResponseWriter, r *http.Request) {
	req := stubReq{Method: r.Method, Path: r.URL.Path}
	if r.Body != nil && !strings.Contains(r.URL.Path, "/download/") {
		if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.Body); err != nil {
				hs.t.Errorf("decode request body %q: %v", raw, err)
			}
		}
	}
	hs.reqs <- req

	hs.mu.Lock()
	for part, status := range hs.fail {
		if strings.Contains(r.URL.Path, part) {
			hs.mu.Unlock()
			w.WriteHeader(status)
			reply(hs.t, w, `{"errcode":"M_FORBIDDEN","error":"refused"}`)
			return
		}
	}
	members, roomName, download := hs.members, hs.roomName, hs.download
	hs.mu.Unlock()

	switch p := r.URL.Path; {
	case strings.Contains(p, "/send/m.room.message/"):
		reply(hs.t, w, `{"event_id":"$sent"}`)
	case strings.Contains(p, "/typing/"):
		reply(hs.t, w, `{}`)
	case strings.HasSuffix(p, "/join"):
		reply(hs.t, w, `{"room_id":"`+stubRoom+`"}`)
	case strings.HasSuffix(p, "/joined_members"):
		joined := map[string]any{}
		for i := range members {
			joined[fmt.Sprintf("@user%d:matrix.test", i)] = map[string]any{}
		}
		out, err := json.Marshal(map[string]any{"joined": joined})
		if err != nil {
			hs.t.Errorf("encode members: %v", err)
		}
		reply(hs.t, w, string(out))
	case strings.Contains(p, "/state/m.room.name"):
		if roomName == "" {
			w.WriteHeader(http.StatusNotFound)
			reply(hs.t, w, `{"errcode":"M_NOT_FOUND","error":"no name"}`)
			return
		}
		reply(hs.t, w, `{"name":"`+roomName+`"}`)
	case strings.Contains(p, "/download/") && download != nil:
		download(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		reply(hs.t, w, `{"errcode":"M_NOT_FOUND","error":"not found"}`)
	}
}

// next returns the next request whose path contains pathPart, skipping others.
func (hs *stubHomeserver) next(t *testing.T, pathPart string) stubReq {
	t.Helper()
	deadline := time.After(failAfter)
	for {
		select {
		case r := <-hs.reqs:
			if strings.Contains(r.Path, pathPart) {
				return r
			}
		case <-deadline:
			t.Fatalf("no request to %q", pathPart)
		}
	}
}

// none fails if any request has been received so far.
func (hs *stubHomeserver) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-hs.reqs:
		t.Fatalf("unexpected request %s %s", r.Method, r.Path)
	default:
	}
}

// stubChannel is a running (not syncing) channel against hs that handles
// every event of the last hour. edit adjusts the configuration.
func stubChannel(t *testing.T, hs *stubHomeserver, mb *bus.MessageBus, edit func(*config.MatrixConfig)) *MatrixChannel {
	t.Helper()
	resetAccounts(t)
	cfg := config.MatrixConfig{
		Homeserver:  hs.url,
		UserID:      stubUser,
		AccessToken: "token",
		AllowFrom:   config.FlexibleStringSlice{"*"},
	}
	if edit != nil {
		edit(&cfg)
	}
	c, err := NewMatrixChannel(cfg, mb)
	if err != nil {
		t.Fatal(err)
	}
	c.startTime = time.Now().Add(-time.Hour)
	c.SetRunning(true)
	return c
}

func TestNewMatrixChannel_RequiresSettings(t *testing.T) {
	ok := config.MatrixConfig{Homeserver: "https://matrix.test", UserID: stubUser, AccessToken: "token"}
	cases := []struct {
		name string
		edit func(*config.MatrixConfig)
		want string
	}{
		{"homeserver", func(c *config.MatrixConfig) { c.Homeserver = "  " }, "matrix homeserver is required"},
		{"user", func(c *config.MatrixConfig) { c.UserID = "" }, "matrix user_id is required"},
		{"token", func(c *config.MatrixConfig) { c.AccessToken = " " }, "matrix access_token is required"},
		{"bad url", func(c *config.MatrixConfig) { c.Homeserver = "http://[::1" }, "create matrix client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ok
			tc.edit(&cfg)
			c, err := NewMatrixChannel(cfg, bus.NewMessageBus())
			if c != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewMatrixChannel = %v, %v; want error %q", c, err, tc.want)
			}
		})
	}
}

func TestNewMatrixChannel_DeviceID(t *testing.T) {
	resetAccounts(t)
	c, err := NewMatrixChannel(config.MatrixConfig{
		Homeserver: "https://matrix.test", UserID: stubUser, AccessToken: "token", DeviceID: "DEV1",
	}, bus.NewMessageBus())
	if err != nil {
		t.Fatal(err)
	}
	if c.client.DeviceID != "DEV1" {
		t.Fatalf("device id = %q, want DEV1", c.client.DeviceID)
	}
}

// Stop on a channel that never started returns at once.
func TestStop_NotStarted(t *testing.T) {
	resetAccounts(t)
	c, err := NewMatrixChannel(config.MatrixConfig{
		Homeserver: "https://matrix.test", UserID: stubUser, AccessToken: "token",
	}, bus.NewMessageBus())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRunning(true)
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.IsRunning() {
		t.Fatal("still running after Stop")
	}
}

// Stop gives up waiting for the sync when its context ends.
func TestStop_DeadlineEndsTheWait(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	c.syncDone = make(chan struct{}) // a sync that never ends
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// A /sync failure mautrix retries itself is reported as an outage; a rejected
// token is returned as fatal without being counted as one.
func TestHealthSyncer_OnFailedSync(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	s := healthSyncer{DefaultSyncer: c.syncer, ch: c}

	wait, fatal := s.OnFailedSync(nil, mautrix.MUnknownToken)
	if !errors.Is(fatal, mautrix.MUnknownToken) || wait != 0 {
		t.Fatalf("unknown token: wait %v fatal %v, want 0 and the error", wait, fatal)
	}
	if !c.ConnDownSince().IsZero() {
		t.Fatal("a rejected token was counted as an outage")
	}

	wait, fatal = s.OnFailedSync(nil, errors.New("bad gateway"))
	if fatal != nil || wait <= 0 {
		t.Fatalf("transient: wait %v fatal %v, want a retry", wait, fatal)
	}
	if c.ConnDownSince().IsZero() {
		t.Fatal("a transient failure was not reported as an outage")
	}
}

func TestSend(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	ctx := context.Background()

	if err := c.Send(ctx, bus.OutboundMessage{ChatID: stubRoom, Content: "  "}); err != nil {
		t.Fatalf("empty content: %v", err)
	}
	if err := c.Send(ctx, bus.OutboundMessage{ChatID: " ", Content: "hi"}); !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("no room: %v, want ErrSendFailed", err)
	}
	hs.none(t)

	if err := c.Send(ctx, bus.OutboundMessage{ChatID: " " + stubRoom + " ", Content: " **hi** "}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	r := hs.next(t, "/send/m.room.message/")
	if !strings.Contains(r.Path, "/rooms/"+stubRoom+"/") {
		t.Errorf("sent to %s, want room %s", r.Path, stubRoom)
	}
	if r.Body["msgtype"] != "m.text" || r.Body["body"] != "**hi**" || r.Body["format"] != "org.matrix.custom.html" ||
		!strings.Contains(fmt.Sprint(r.Body["formatted_body"]), "<strong>hi</strong>") {
		t.Errorf("sent %v", r.Body)
	}

	hs.setFail("/send/", http.StatusForbidden)
	if err := c.Send(ctx, bus.OutboundMessage{ChatID: stubRoom, Content: "hi"}); !errors.Is(err, channels.ErrTemporary) {
		t.Fatalf("server failure: %v, want ErrTemporary", err)
	}

	c.SetRunning(false)
	if err := c.Send(ctx, bus.OutboundMessage{ChatID: stubRoom, Content: "hi"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("stopped: %v, want ErrNotRunning", err)
	}
}

func TestSendPlaceholder(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		if id, err := c.SendPlaceholder(ctx, stubRoom); id != "" || err != nil {
			t.Fatalf("SendPlaceholder = %q, %v; want nothing", id, err)
		}
		hs.none(t)
	})

	for _, tc := range []struct{ text, want string }{
		{"", "Thinking... 💭"},
		{" Working ", "Working"},
	} {
		t.Run("text "+tc.want, func(t *testing.T) {
			hs := newStubHomeserver(t)
			c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) {
				cfg.Placeholder = config.PlaceholderConfig{Enabled: true, Text: tc.text}
			})
			got, err := c.SendPlaceholder(ctx, stubRoom)
			if err != nil || got != "$sent" {
				t.Fatalf("SendPlaceholder = %q, %v; want $sent", got, err)
			}
			r := hs.next(t, "/send/m.room.message/")
			if r.Body["msgtype"] != "m.notice" || r.Body["body"] != tc.want {
				t.Fatalf("sent %v, want a notice %q", r.Body, tc.want)
			}
		})
	}

	t.Run("errors", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) { cfg.Placeholder.Enabled = true })
		if _, err := c.SendPlaceholder(ctx, "  "); err == nil {
			t.Fatal("no room: want an error")
		}
		hs.setFail("/send/", http.StatusForbidden)
		if got, err := c.SendPlaceholder(ctx, stubRoom); err == nil || got != "" {
			t.Fatalf("server failure: %q, %v; want an error", got, err)
		}
	})
}

func TestEditMessage(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) { cfg.MessageFormat = "plain" })
	ctx := context.Background()

	if err := c.EditMessage(ctx, " ", "$orig", "x"); err == nil {
		t.Fatal("no room: want an error")
	}
	if err := c.EditMessage(ctx, stubRoom, " ", "x"); err == nil {
		t.Fatal("no message id: want an error")
	}
	hs.none(t)

	if err := c.EditMessage(ctx, stubRoom, "$orig", "new text"); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	r := hs.next(t, "/send/m.room.message/")
	rel, relOK := r.Body["m.relates_to"].(map[string]any)
	nc, ncOK := r.Body["m.new_content"].(map[string]any)
	if !relOK || !ncOK || rel["rel_type"] != "m.replace" || rel["event_id"] != "$orig" || nc["body"] != "new text" || nc["format"] != nil {
		t.Fatalf("edit sent %v", r.Body)
	}

	hs.setFail("/send/", http.StatusForbidden)
	if err := c.EditMessage(ctx, stubRoom, "$orig", "x"); err == nil {
		t.Fatal("server failure: want an error")
	}
}

func TestStartTyping(t *testing.T) {
	ctx := context.Background()

	t.Run("not running", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		c.SetRunning(false)
		stop, err := c.StartTyping(ctx, stubRoom)
		if err != nil {
			t.Fatal(err)
		}
		stop()
		hs.none(t)
	})

	t.Run("no room", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		if _, err := c.StartTyping(ctx, " "); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("start and stop", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		stop, err := c.StartTyping(ctx, stubRoom)
		if err != nil {
			t.Fatal(err)
		}
		if r := hs.next(t, "/typing/"); r.Method != http.MethodPut || r.Body["typing"] != true {
			t.Fatalf("start sent %s %v, want typing on", r.Method, r.Body)
		}
		stop()
		stop() // a second stop does nothing
		if r := hs.next(t, "/typing/"); r.Body["typing"] != false {
			t.Fatalf("stop sent %v, want typing off", r.Body)
		}
		c.typingMu.Lock()
		n := len(c.typingSessions)
		c.typingMu.Unlock()
		if n != 0 {
			t.Fatalf("%d typing sessions left after stop", n)
		}
	})

	// A second start in the same room replaces the first; the first's stop
	// then leaves the second in place.
	t.Run("restart in a room", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		stop1, err := c.StartTyping(ctx, stubRoom)
		if err != nil {
			t.Fatal(err)
		}
		hs.next(t, "/typing/")
		stop2, err := c.StartTyping(ctx, stubRoom)
		if err != nil {
			t.Fatal(err)
		}
		hs.next(t, "/typing/")
		stop1()
		hs.next(t, "/typing/")
		c.typingMu.Lock()
		kept := c.typingSessions[stubRoom]
		c.typingMu.Unlock()
		if kept == nil {
			t.Fatal("the first stop removed the second session")
		}
		stop2()
		hs.next(t, "/typing/")
	})

	// Stop clears every room still showing typing; a failure to clear one is
	// only logged.
	t.Run("channel stop clears typing", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		if _, err := c.StartTyping(ctx, stubRoom); err != nil {
			t.Fatal(err)
		}
		hs.next(t, "/typing/")
		hs.setFail("/typing/", http.StatusForbidden)
		if err := c.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if r := hs.next(t, "/typing/"); r.Body["typing"] != false {
			t.Fatalf("stop sent %v, want typing off", r.Body)
		}
	})

	// A failing typing call is logged and typing still stops cleanly.
	t.Run("server refuses", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		hs.setFail("/typing/", http.StatusForbidden)
		stop, err := c.StartTyping(ctx, stubRoom)
		if err != nil {
			t.Fatal(err)
		}
		hs.next(t, "/typing/")
		stop()
		hs.next(t, "/typing/")
	})

	// The typing loop ends with its context.
	t.Run("context ends the loop", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		loopCtx, cancel := context.WithCancel(ctx)
		session := newTypingSession()
		done := make(chan struct{})
		go func() {
			c.typingLoop(loopCtx, stubRoom, session)
			close(done)
		}()
		hs.next(t, "/typing/")
		cancel()
		select {
		case <-done:
		case <-time.After(failAfter):
			t.Fatal("typing loop did not end with its context")
		}
	})
}

func memberEvent(membership event.Membership, stateKey string) *event.Event {
	return &event.Event{
		Type:     event.StateMember,
		RoomID:   stubRoom,
		StateKey: &stateKey,
		Content:  event.Content{Parsed: &event.MemberEventContent{Membership: membership}},
	}
}

func TestHandleMemberEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("join on invite off", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, nil)
		c.handleMemberEvent(ctx, memberEvent(event.MembershipInvite, stubUser))
		hs.none(t)
	})

	t.Run("ignored", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) { cfg.JoinOnInvite = true })
		c.handleMemberEvent(ctx, nil)
		c.handleMemberEvent(ctx, memberEvent(event.MembershipJoin, stubUser))
		c.handleMemberEvent(ctx, memberEvent(event.MembershipInvite, "@bob:matrix.test"))
		hs.none(t)
	})

	t.Run("joins", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) { cfg.JoinOnInvite = true })
		c.handleMemberEvent(ctx, memberEvent(event.MembershipInvite, stubUser))
		if r := hs.next(t, "/join"); r.Method != http.MethodPost || !strings.Contains(r.Path, stubRoom) {
			t.Fatalf("join request %s %s", r.Method, r.Path)
		}
	})

	t.Run("join refused", func(t *testing.T) {
		hs := newStubHomeserver(t)
		c := stubChannel(t, hs, nil, func(cfg *config.MatrixConfig) { cfg.JoinOnInvite = true })
		hs.setFail("/join", http.StatusForbidden)
		c.handleMemberEvent(ctx, memberEvent(event.MembershipInvite, stubUser))
		hs.next(t, "/join")
	})
}

var eventSeq int

// msgEvent is a message event from Alice in stubRoom, sent now.
func msgEvent(content *event.MessageEventContent) *event.Event {
	eventSeq++
	return &event.Event{
		Type:      event.EventMessage,
		RoomID:    stubRoom,
		Sender:    "@alice:matrix.test",
		ID:        id.EventID(fmt.Sprintf("$ev%d", eventSeq)),
		Timestamp: time.Now().UnixMilli(),
		Content:   event.Content{Parsed: content},
	}
}

func textMsg(body string) *event.MessageEventContent {
	return &event.MessageEventContent{MsgType: event.MsgText, Body: body}
}

// consumeOne returns the next inbound message.
func consumeOne(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), failAfter)
	defer cancel()
	msg, ok := mb.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("no inbound message")
	}
	return msg
}

// A direct message is handed on with its sender, room and reply metadata, and
// a mention of the bot removed.
func TestHandleMessageEvent_DirectMessage(t *testing.T) {
	hs := newStubHomeserver(t)
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := stubChannel(t, hs, mb, nil)

	msg := textMsg(stubUser + ": hello there")
	msg.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$parent"}}
	evt := msgEvent(msg)
	c.handleMessageEvent(context.Background(), evt)

	got := consumeOne(t, mb)
	if got.Content != "hello there" || got.ChatID != stubRoom || got.Peer.Kind != "direct" ||
		got.Peer.ID != "@alice:matrix.test" || got.MessageID != evt.ID.String() {
		t.Fatalf("inbound %+v", got)
	}
	if got.Metadata["reply_to_msg_id"] != "$parent" || got.Metadata["is_group"] != "false" ||
		got.Metadata["room_id"] != stubRoom || got.Metadata["sender_raw"] != "@alice:matrix.test" {
		t.Fatalf("metadata %v", got.Metadata)
	}
	if !c.account.handled(evt.ID) {
		t.Fatal("a handed-on event is not recorded as handled")
	}

	// The same event again (a repeat in a later sync) is not handed on.
	c.handleMessageEvent(context.Background(), evt)
	if rest := drainInbound(t, mb); len(rest) != 0 {
		t.Fatalf("repeat delivered %q", rest)
	}
}

// Events the channel must not hand on.
func TestHandleMessageEvent_Ignored(t *testing.T) {
	hs := newStubHomeserver(t)
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := stubChannel(t, hs, mb, func(cfg *config.MatrixConfig) {
		cfg.AllowFrom = config.FlexibleStringSlice{"@bob:matrix.test"}
	})
	ctx := context.Background()

	own := msgEvent(textMsg("mine"))
	own.Sender = stubUser
	old := msgEvent(textMsg("old"))
	old.Timestamp = c.startTime.Add(-time.Second).UnixMilli()
	edit := textMsg("edited")
	edit.RelatesTo = &event.RelatesTo{Type: event.RelReplace, EventID: "$orig"}
	bobOnlyMention := msgEvent(textMsg(stubUser))
	bobOnlyMention.Sender = "@bob:matrix.test"
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	bobCancelled := msgEvent(textMsg("too late"))
	bobCancelled.Sender = "@bob:matrix.test"

	c.handleMessageEvent(ctx, nil)
	c.handleMessageEvent(ctx, own)
	c.handleMessageEvent(ctx, old)
	c.handleMessageEvent(ctx, msgEvent(edit))
	c.handleMessageEvent(ctx, msgEvent(&event.MessageEventContent{MsgType: event.MsgLocation, Body: "geo"}))
	c.handleMessageEvent(ctx, msgEvent(textMsg("   ")))
	c.handleMessageEvent(ctx, msgEvent(textMsg("not allowed"))) // Alice is not on the allow list
	c.handleMessageEvent(ctx, bobOnlyMention)                   // nothing left once the mention is removed
	c.handleMessageEvent(cancelled, bobCancelled)               // the channel is stopping

	if got := drainInbound(t, mb); len(got) != 0 {
		t.Fatalf("delivered %q, want nothing", got)
	}
	if c.account.handled(bobCancelled.ID) {
		t.Fatal("an event not handed on while stopping was recorded as handled")
	}
}

// In a group room the trigger rules apply; a mention is answered with the
// mention removed.
func TestHandleMessageEvent_GroupRoom(t *testing.T) {
	hs := newStubHomeserver(t)
	hs.members = 3
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := stubChannel(t, hs, mb, func(cfg *config.MatrixConfig) {
		cfg.GroupTrigger = config.GroupTriggerConfig{MentionOnly: true}
	})
	ctx := context.Background()

	c.handleMessageEvent(ctx, msgEvent(textMsg("chatter")))
	mention := textMsg("@agency what time is it")
	mention.Mentions = &event.Mentions{UserIDs: []id.UserID{stubUser}}
	c.handleMessageEvent(ctx, msgEvent(mention))

	got := consumeOne(t, mb)
	if got.Content != "what time is it" || got.Peer.Kind != "group" || got.Peer.ID != stubRoom || got.Metadata["is_group"] != "true" {
		t.Fatalf("inbound %+v", got)
	}
	if rest := drainInbound(t, mb); len(rest) != 0 {
		t.Fatalf("also delivered %q", rest)
	}
	// The room kind was looked up once and then cached.
	var lookups int
	for {
		select {
		case r := <-hs.reqs:
			if strings.HasSuffix(r.Path, "/joined_members") {
				lookups++
			}
			continue
		default:
		}
		break
	}
	if lookups != 1 {
		t.Fatalf("joined-members lookups = %d, want 1", lookups)
	}
}

// A room whose members cannot be listed is taken as a direct chat.
func TestIsGroupRoom_LookupFailureIsDirect(t *testing.T) {
	hs := newStubHomeserver(t)
	hs.members = 5
	hs.setFail("/joined_members", http.StatusForbidden)
	c := stubChannel(t, hs, nil, nil)
	if c.isGroupRoom(context.Background(), stubRoom) {
		t.Fatal("a failed lookup was taken as a group")
	}
	if _, ok := c.roomKindCache.get(stubRoom, time.Now()); ok {
		t.Fatal("a failed lookup was cached")
	}
}

// failingStore is a media store whose Store always fails.
type failingStore struct{ media.MediaStore }

func (failingStore) Store(string, media.MediaMeta, string) (string, error) {
	return "", errors.New("store full")
}

// An image is downloaded and handed on as a media ref under the event's scope.
func TestHandleMessageEvent_ImageDownloaded(t *testing.T) {
	hs := newStubHomeserver(t)
	hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, "PNGDATA") }
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := stubChannel(t, hs, mb, nil)
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)

	evt := msgEvent(&event.MessageEventContent{
		MsgType: event.MsgImage, Body: "cat", URL: "mxc://matrix.test/abc",
		Info: &event.FileInfo{MimeType: "image/png"},
	})
	c.handleMessageEvent(context.Background(), evt)

	got := consumeOne(t, mb)
	if got.Content != "[image: cat]" || len(got.Media) != 1 || !strings.HasPrefix(got.Media[0], "media://") {
		t.Fatalf("inbound %+v", got)
	}
	path, meta, err := store.ResolveWithMeta(got.Media[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFile(t, path) })
	if meta.Filename != "cat.png" || meta.ContentType != "image/png" || meta.Source != "matrix" {
		t.Errorf("meta %+v", meta)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "PNGDATA" {
		t.Errorf("stored %q, %v", data, err)
	}
}

// Without a usable media store the downloaded file's own path is handed on.
func TestStoreMedia_FallsBackToLocalPath(t *testing.T) {
	c := &MatrixChannel{BaseChannel: channels.NewBaseChannel("matrix", nil, nil, nil)}
	if got := c.storeMedia("/tmp/x.png", media.MediaMeta{}, "s"); got != "/tmp/x.png" {
		t.Fatalf("no store: %q", got)
	}
	c.SetMediaStore(failingStore{})
	if got := c.storeMedia("/tmp/x.png", media.MediaMeta{}, "s"); got != "/tmp/x.png" {
		t.Fatalf("failing store: %q", got)
	}
}

// A media message whose download fails is handed on as its text marker.
func TestExtractInboundMedia_DownloadFailureKeepsMarker(t *testing.T) {
	hs := newStubHomeserver(t)
	hs.setFail("/download/", http.StatusNotFound)
	c := stubChannel(t, hs, nil, nil)
	msg := &event.MessageEventContent{
		MsgType: event.MsgVideo, Body: "clip.mp4", URL: "mxc://matrix.test/v",
	}
	content, ok := inboundText(msg)
	refs := c.fetchInboundMedia(context.Background(), msg, "scope")
	if !ok || content != "[video: clip.mp4]" || len(refs) != 0 {
		t.Fatalf("got %q %v %v", content, refs, ok)
	}
}

// encryptedFile encrypts plaintext as a Matrix attachment at uri and returns
// the file info as a receiver decodes it from the event.
func encryptedFile(t *testing.T, plaintext []byte, uri string) (*event.EncryptedFileInfo, []byte) {
	t.Helper()
	ef := attachment.NewEncryptedFile()
	ciphertext := slices.Clone(plaintext)
	ef.EncryptInPlace(ciphertext)
	raw, err := json.Marshal(event.EncryptedFileInfo{EncryptedFile: *ef, URL: id.ContentURIString(uri)})
	if err != nil {
		t.Fatal(err)
	}
	var info event.EncryptedFileInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	return &info, ciphertext
}

func TestDownloadMedia_EncryptedAttachment(t *testing.T) {
	file, ciphertext := encryptedFile(t, []byte("secret audio"), "mxc://matrix.test/enc")

	t.Run("decrypted", func(t *testing.T) {
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, string(ciphertext)) }
		c := stubChannel(t, hs, nil, nil)
		path, err := c.downloadMedia(context.Background(), &event.MessageEventContent{
			MsgType: event.MsgAudio, Body: "voice", File: file,
		}, "audio")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { removeFile(t, path) })
		if filepath.Ext(path) != ".ogg" {
			t.Errorf("extension %q, want .ogg", filepath.Ext(path))
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "secret audio" {
			t.Fatalf("decrypted %q, %v", data, err)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		tampered := slices.Clone(ciphertext)
		tampered[0] ^= 0xff
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, string(tampered)) }
		c := stubChannel(t, hs, nil, nil)
		before := mediaTempFiles(t)
		_, err := c.downloadMedia(context.Background(), &event.MessageEventContent{
			MsgType: event.MsgAudio, Body: "voice", File: file,
		}, "audio")
		if !errors.Is(err, attachment.ErrHashMismatch) {
			t.Fatalf("err = %v, want a hash mismatch", err)
		}
		if after := mediaTempFiles(t); after > before {
			t.Fatal("a rejected download left its temp file")
		}
	})

	t.Run("bad key", func(t *testing.T) {
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, string(ciphertext)) }
		c := stubChannel(t, hs, nil, nil)
		bad := *file
		bad.Key.Algorithm = "A128CBC"
		_, err := c.downloadMedia(context.Background(), &event.MessageEventContent{
			MsgType: event.MsgAudio, File: &bad,
		}, "audio")
		if err == nil || !strings.Contains(err.Error(), "decrypt matrix media") {
			t.Fatalf("err = %v, want a decrypt error", err)
		}
	})
}

// removeFile removes a file a test produced.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Error(err)
	}
}

// mediaTempFiles counts the channel's temp media files.
func mediaTempFiles(t *testing.T) int {
	t.Helper()
	dir, err := matrixMediaTempDir()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "matrix-media-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func TestDownloadMedia_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid url", func(t *testing.T) {
		c := &MatrixChannel{}
		_, err := c.downloadMedia(ctx, &event.MessageEventContent{URL: "https://example.org/x"}, "image")
		if err == nil || !strings.Contains(err.Error(), "invalid matrix media URL") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			reply(t, w, "short")
		}
		c := stubChannel(t, hs, nil, nil)
		before := mediaTempFiles(t)
		if _, err := c.downloadMedia(ctx, &event.MessageEventContent{URL: "mxc://matrix.test/t"}, "file"); err == nil {
			t.Fatal("want an error for a truncated body")
		}
		if after := mediaTempFiles(t); after > before {
			t.Fatal("a failed download left its temp file")
		}
	})

	t.Run("temp dir unusable", func(t *testing.T) {
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, "x") }
		c := stubChannel(t, hs, nil, nil)
		notDir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notDir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", notDir)
		_, err := c.downloadMedia(ctx, &event.MessageEventContent{URL: "mxc://matrix.test/t"}, "file")
		if err == nil || !strings.Contains(err.Error(), "create matrix media directory") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("temp dir not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		hs := newStubHomeserver(t)
		hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, "x") }
		c := stubChannel(t, hs, nil, nil)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		dir, err := matrixMediaTempDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Error(err)
			}
		})
		if _, err := c.downloadMedia(ctx, &event.MessageEventContent{URL: "mxc://matrix.test/t"}, "file"); err == nil {
			t.Fatal("want an error when the temp file cannot be created")
		}
	})
}

func TestMatrixMediaHelpers(t *testing.T) {
	file := &event.EncryptedFileInfo{URL: "mxc://matrix.test/f"}
	if got := matrixMediaURI(nil); got != "" {
		t.Errorf("URI(nil) = %q", got)
	}
	if got := matrixMediaURI(&event.MessageEventContent{File: file}); got != "mxc://matrix.test/f" {
		t.Errorf("URI(file) = %q", got)
	}
	if got := matrixMediaURI(&event.MessageEventContent{}); got != "" {
		t.Errorf("URI(none) = %q", got)
	}
	if got := matrixContentType(nil); got != "" {
		t.Errorf("content type(nil) = %q", got)
	}

	for msgType, want := range map[event.MessageType]string{
		event.MsgImage: "image", event.MsgAudio: "audio", event.MsgVideo: "video", event.MsgFile: "file",
	} {
		if got := matrixMediaKind(msgType); got != want {
			t.Errorf("kind(%s) = %q, want %q", msgType, got, want)
		}
	}

	if got := matrixMediaLabel(nil, "image"); got != "image" {
		t.Errorf("label(nil) = %q", got)
	}
	if got := matrixMediaLabel(&event.MessageEventContent{Body: " "}, "file"); got != "file" {
		t.Errorf("label(empty) = %q", got)
	}

	for _, tc := range []struct{ label, kind, ct, want string }{
		{"report.pdf", "file", "", "report.pdf"},
		{"", "audio", "", "audio.ogg"},
		{"photo", "image", "image/png", "photo.png"},
	} {
		if got := matrixMediaFilename(tc.label, tc.kind, tc.ct); got != tc.want {
			t.Errorf("filename(%q,%q,%q) = %q, want %q", tc.label, tc.kind, tc.ct, got, tc.want)
		}
	}
}

func TestMatrixOutboundMsgType_AllRules(t *testing.T) {
	cases := []struct {
		partType, filename, contentType string
		want                            event.MessageType
	}{
		{"voice", "", "", event.MsgAudio},
		{"video", "", "", event.MsgVideo},
		{"document", "", "", event.MsgFile},
		{"", "", "image/png", event.MsgImage},
		{"", "", "audio/mpeg", event.MsgAudio},
		{"", "", "application/ogg", event.MsgAudio},
		{"", "x.webp", "", event.MsgImage},
		{"", "x.mkv", "", event.MsgVideo},
	}
	for _, tc := range cases {
		if got := matrixOutboundMsgType(tc.partType, tc.filename, tc.contentType); got != tc.want {
			t.Errorf("(%q,%q,%q) = %q, want %q", tc.partType, tc.filename, tc.contentType, got, tc.want)
		}
	}
	if got := matrixOutboundContent("", "", event.MsgVideo, "", 0, ""); got.Body != "video" {
		t.Errorf("body with no caption or name = %q, want the media kind", got.Body)
	}
}

func TestMentionDetection_EdgeCases(t *testing.T) {
	if mentionsUserInFormattedBody("anything", "") {
		t.Error("an empty user id matched")
	}
	if mentionsUserInFormattedBody("  ", stubUser) {
		t.Error("an empty body matched")
	}
	cases := []struct {
		body string
		want bool
	}{
		{`<a href="https://matrix.to/#/` + stubUser + `">x</a>`, true},
		{`<a href="https://matrix.to/#/%2540agency%253Amatrix.test">x</a>`, true},
		{`<a href="https://matrix.to/#/%25252540agency%2525253Amatrix.test">x</a>`, true},
		{`<a href="https://matrix.to/#/%2525252540agency%252525253Amatrix.test">x</a>`, true},
		{`<a href="%zz">x</a>`, false},
		{`<a href="https://matrix.to/#/@bob:matrix.test">x</a>`, false},
	}
	for _, tc := range cases {
		if got := mentionsUserInFormattedBody(tc.body, stubUser); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.body, got, tc.want)
		}
	}
	if decodeMatrixMentionHref("  ") != "" {
		t.Error("an empty href decoded to something")
	}

	c := &MatrixChannel{client: &mautrix.Client{UserID: stubUser}}
	if c.isBotMentioned(nil) {
		t.Error("nil message mentioned the bot")
	}
	// Without a precompiled localpart pattern one is built.
	if !c.isBotMentioned(&event.MessageEventContent{Body: "hi @agency"}) {
		t.Error("localpart mention missed")
	}
	anon := &MatrixChannel{client: &mautrix.Client{}}
	if anon.isBotMentioned(&event.MessageEventContent{Body: "hi @agency"}) {
		t.Error("a bot with no user id was mentioned")
	}
}

func TestRoomKindCache_UpdateAndCleanup(t *testing.T) {
	def := newRoomKindCache(0, 0)
	if def.maxEntries != roomKindCacheMaxEntries || def.ttl != roomKindCacheTTL {
		t.Fatalf("defaults %d %v", def.maxEntries, def.ttl)
	}

	cache := newRoomKindCache(2, time.Minute)
	now := time.Unix(100, 0)
	cache.set("!a", false, now)
	cache.set("!a", true, now.Add(30*time.Second)) // updates, extends the ttl
	if got, ok := cache.get("!a", now.Add(80*time.Second)); !ok || !got {
		t.Fatalf("updated entry: ok=%v group=%v", ok, got)
	}

	cache.set("!b", false, now)
	if n := cache.cleanupExpired(now.Add(70 * time.Second)); n != 1 {
		t.Fatalf("cleanup removed %d, want 1", n)
	}
	if _, ok := cache.get("!b", now); ok {
		t.Fatal("expired entry kept")
	}

	// A full cache drops expired entries before evicting live ones.
	full := newRoomKindCache(2, time.Minute)
	full.set("!old", false, now)
	full.set("!live", false, now.Add(50*time.Second))
	full.set("!new", true, now.Add(65*time.Second))
	if _, ok := full.get("!live", now.Add(65*time.Second)); !ok {
		t.Fatal("a live entry was evicted while an expired one could go")
	}
}

// The janitor goroutine ends with its context.
func TestRunRoomKindCacheJanitor_EndsWithContext(t *testing.T) {
	c := &MatrixChannel{roomKindCache: newRoomKindCache(1, time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runRoomKindCacheJanitor(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(failAfter):
		t.Fatal("janitor did not stop")
	}
}

func TestLookupRoomName(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	ctx := context.Background()
	if got := c.lookupRoomName(ctx, stubRoom); got != "" {
		t.Fatalf("unnamed room = %q", got)
	}
	hs.mu.Lock()
	hs.roomName = "Alice's room"
	hs.mu.Unlock()
	if got := c.lookupRoomName(ctx, stubRoom); got != "Alice's room" {
		t.Fatalf("named room = %q", got)
	}
}

func TestHandleEncryptedEvent_IgnoresEventsWithoutRoom(t *testing.T) {
	resetEncryptedRooms(t)
	c := &MatrixChannel{roomName: func(context.Context, id.RoomID) string {
		t.Fatal("looked up a room name")
		return ""
	}}
	c.handleEncryptedEvent(context.Background(), nil)
	c.handleEncryptedEvent(context.Background(), &event.Event{Type: event.EventEncrypted})
	if got := EncryptedRooms(); len(got) != 0 {
		t.Fatalf("recorded %q", got)
	}
}

// A batch whose handling failed is not recorded as processed: the next
// channel fetches it again.
func TestHealthSyncer_FailedBatchNotRecorded(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	c.syncer.OnEventType(event.EventMessage, func(context.Context, *event.Event) { panic("handler failed") })
	var res mautrix.RespSync
	if err := json.Unmarshal([]byte(syncBody("s9", textEvent("$p", "boom", time.Now()))), &res); err != nil {
		t.Fatal(err)
	}
	s := healthSyncer{DefaultSyncer: c.syncer, ch: c}
	if err := s.ProcessResponse(context.Background(), &res, ""); err == nil {
		t.Fatal("ProcessResponse = nil, want the handler's failure")
	}
	if from, err := (accountSyncStore{c.account}).LoadNextBatch(context.Background(), ""); err != nil || from != "" {
		t.Fatalf("resume point = %q, want the failed batch fetched again", from)
	}
}

// Without a file name anywhere the stored file's own name is used, and a name
// without an extension is sent as generic binary data.
func TestSendMedia_NameAndTypeFallbacks(t *testing.T) {
	srv := &matrixMediaServer{}
	c, store := newMediaTestChannel(t, srv)
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(p, media.MediaMeta{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: stubRoom, Parts: []bus.MediaPart{{Ref: ref}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(srv.uploads) != 1 || srv.uploads[0] != "blob|application/octet-stream|raw" {
		t.Fatalf("uploads = %v", srv.uploads)
	}
	if srv.events[0]["msgtype"] != "m.file" || srv.events[0]["body"] != "blob" {
		t.Fatalf("event = %v", srv.events[0])
	}
}

// Claiming an event twice succeeds once.
func TestAccountState_ClaimTwice(t *testing.T) {
	st := &accountState{seen: map[id.EventID]struct{}{}}
	if !st.claim("$a") || st.claim("$a") {
		t.Fatal("an event was claimed twice, or not at all")
	}
}
