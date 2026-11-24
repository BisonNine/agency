package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/utils"
)

// newEventChannel is a running Slack channel with a Socket Mode client on a
// fake Slack API that answers each method with replies[method] (default
// {"ok":true}); inbound messages go to the returned bus. allow is the allow
// list ("*" when nil).
func newEventChannel(t *testing.T, cfg config.SlackConfig, allow []string, replies map[string]string) (*SlackChannel, *bus.MessageBus, *fakeSlackAPI) {
	t.Helper()
	api := &fakeSlackAPI{replies: replies}
	srv := api.serve(t)
	if allow == nil {
		allow = []string{"*"}
	}
	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)
	client := slack.New("xoxb-test", slack.OptionAppLevelToken("xapp-test"), slack.OptionAPIURL(srv.URL+"/"))
	c := &SlackChannel{
		BaseChannel: channels.NewBaseChannel("slack", cfg, mb, allow,
			channels.WithGroupTrigger(cfg.GroupTrigger)),
		config:       cfg,
		api:          client,
		socketClient: socketmode.New(client),
		botUserID:    "UBOT",
		teamID:       "T1",
	}
	c.userNameCache.Store("U1", slackUserName{displayName: "Alice", username: "alice"})
	c.ctx, c.cancel = context.WithCancel(context.Background())
	t.Cleanup(c.cancel)
	c.SetRunning(true)
	return c, mb, api
}

// marker is a DM the channel always publishes; tests send it after messages
// that must be dropped, so the next message on the bus proves the drops.
func marker(c *SlackChannel) {
	c.handleMessageEvent(&slackevents.MessageEvent{User: "U1", Channel: "D1", Text: "marker", TimeStamp: "9.9999"})
}

func expectMarker(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	if msg := nextInbound(t, mb); msg.Content != "marker" {
		t.Fatalf("published %q (%q), want only the marker", msg.MessageID, msg.Content)
	}
}

// Start checks the token, learns the bot's ids and runs; Stop ends its
// context and marks it stopped.
func TestStartStop(t *testing.T) {
	c, _, api := newEventChannel(t, config.SlackConfig{}, nil, map[string]string{
		"auth.test": `{"ok":true,"user_id":"UB2","team_id":"T2","team":"acme"}`,
		// Socket Mode refuses the app token, so the socket loop waits.
		"apps.connections.open": `{"ok":false,"error":"invalid_auth"}`,
	})
	c.SetRunning(false)
	c.botUserID, c.teamID = "", ""

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v", err)
	}
	if !c.IsRunning() || c.botUserID != "UB2" || c.teamID != "T2" {
		t.Fatalf("running %v bot %q team %q, want running UB2 T2", c.IsRunning(), c.botUserID, c.teamID)
	}
	waitFor(t, "the socket to be opened", func() bool { return len(api.callsTo("apps.connections.open")) > 0 })

	ctx := c.ctx
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if c.IsRunning() {
		t.Error("running after Stop")
	}
	if ctx.Err() == nil {
		t.Error("channel context not cancelled by Stop")
	}

	// A channel that never started stops cleanly too.
	idle := &SlackChannel{BaseChannel: channels.NewBaseChannel("slack", nil, nil, nil)}
	if err := idle.Stop(context.Background()); err != nil {
		t.Errorf("Stop of an idle channel = %v", err)
	}
}

// A token Slack rejects fails the start.
func TestStart_AuthRejected(t *testing.T) {
	c, _, _ := newEventChannel(t, config.SlackConfig{}, nil, map[string]string{
		"auth.test": `{"ok":false,"error":"invalid_auth"}`,
	})
	c.SetRunning(false)
	err := c.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("Start = %v, want the invalid_auth error", err)
	}
	if c.IsRunning() {
		t.Error("running after a rejected start")
	}
}

// The event loop routes each Socket Mode event: connection state to the
// outage tracker, Events API messages and slash commands to the agents, and
// it acknowledges interactive requests. It ends when the events stop.
func TestEventLoop_RoutesEvents(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{}, nil, nil)
	events := c.socketClient.Events

	events <- socketmode.Event{Type: socketmode.EventTypeConnectionError, Data: &slack.ConnectionErrorEvent{ErrorObj: errors.New("dial failed")}}
	events <- socketmode.Event{
		Type: socketmode.EventTypeEventsAPI, Request: &socketmode.Request{EnvelopeID: "e1"},
		Data: slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{User: "U1", Channel: "D1", Text: "hi", TimeStamp: "1.0001"},
		}},
	}
	events <- socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> there", TimeStamp: "1.0002"},
		}},
	}
	events <- slashEvent("/agency", "launch", "C1")
	events <- socketmode.Event{Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{EnvelopeID: "e2"}}
	events <- socketmode.Event{Type: socketmode.EventTypeInteractive}
	close(events)

	c.eventLoop()
	if c.ConnDownSince().IsZero() {
		t.Error("connection error not tracked as an outage")
	}
	for _, want := range []struct{ content, chat string }{{"hi", "D1"}, {"there", "C1/1.0002"}, {"launch", "C1"}} {
		if msg := nextInbound(t, mb); msg.Content != want.content || msg.ChatID != want.chat {
			t.Errorf("published %q to %q, want %q to %q", msg.Content, msg.ChatID, want.content, want.chat)
		}
	}

	// A connected event ends the outage.
	c2, _, _ := newEventChannel(t, config.SlackConfig{}, nil, nil)
	c2.ReportConnFailure(errors.New("down"))
	c2.socketClient.Events <- socketmode.Event{Type: socketmode.EventTypeConnected}
	close(c2.socketClient.Events)
	c2.eventLoop()
	if !c2.ConnDownSince().IsZero() {
		t.Error("connected event did not end the outage")
	}

	// A stopped channel's loop returns.
	c3, _, _ := newEventChannel(t, config.SlackConfig{}, nil, nil)
	c3.cancel()
	c3.eventLoop()
}

// Events API envelopes that carry no message, or an unknown inner event,
// publish nothing.
func TestHandleEventsAPI_Ignored(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{}, nil, nil)
	c.handleEventsAPI(socketmode.Event{Data: "not an events API event"})
	c.handleEventsAPI(socketmode.Event{Data: slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{
		Data: &slackevents.ReactionAddedEvent{User: "U1"},
	}}})
	marker(c)
	expectMarker(t, mb)
}

// Messages the channel must not hand on.
func TestHandleMessageEvent_Dropped(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{GroupTrigger: config.GroupTriggerConfig{MentionOnly: true}},
		[]string{"U1"}, nil)

	for _, ev := range []*slackevents.MessageEvent{
		{User: "UBOT", Channel: "D1", Text: "my own", TimeStamp: "1.1"},
		{User: "", Channel: "D1", Text: "no user", TimeStamp: "1.2"},
		{User: "U1", BotID: "B1", Channel: "D1", Text: "a bot", TimeStamp: "1.3"},
		{User: "U1", SubType: "message_changed", Channel: "D1", Text: "edited", TimeStamp: "1.4"},
		{User: "U2", Channel: "D1", Text: "not allowed", TimeStamp: "1.5"},
		{User: "U1", Channel: "C1", Text: "not mentioned", TimeStamp: "1.6"},
		{User: "U1", Channel: "D1", Text: "<@UBOT>  ", TimeStamp: "1.7"},
	} {
		c.handleMessageEvent(ev)
	}
	marker(c)
	expectMarker(t, mb)
}

// A channel message with a prefix trigger loses the prefix; a thread reply's
// chat is the thread; metadata says where it came from.
func TestHandleMessageEvent_ChannelThread(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{GroupTrigger: config.GroupTriggerConfig{Prefixes: []string{"!ask"}}}, nil, nil)

	c.handleMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "C1", Text: "!ask what time", TimeStamp: "2.0002", ThreadTimeStamp: "2.0001",
	})
	msg := nextInbound(t, mb)
	if msg.Content != "what time" || msg.ChatID != "C1/2.0001" || msg.MessageID != "2.0002" {
		t.Fatalf("got %q in %q (%q), want %q in C1/2.0001 (2.0002)", msg.Content, msg.ChatID, msg.MessageID, "what time")
	}
	if msg.Peer.Kind != "channel" || msg.Peer.ID != "C1" {
		t.Errorf("peer = %+v, want channel C1", msg.Peer)
	}
	want := map[string]string{"message_ts": "2.0002", "channel_id": "C1", "thread_ts": "2.0001", "platform": "slack", "team_id": "T1"}
	for k, v := range want {
		if msg.Metadata[k] != v {
			t.Errorf("metadata[%s] = %q, want %q", k, msg.Metadata[k], v)
		}
	}
	if _, ok := c.pendingAcks.Load("2.0002"); !ok {
		t.Error("message not waiting for its acknowledgement")
	}
}

// Shared files are downloaded with the bot token and passed on (through the
// media store when there is one); a file without a URL or that cannot be
// downloaded is skipped.
func TestHandleMessageEvent_Files(t *testing.T) {
	utils.SetMediaStagingDir(t.TempDir(), 0, 0)
	t.Cleanup(func() { utils.SetMediaStagingDir("", 0, 0) })
	var mu sync.Mutex
	var auth []string
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path == "/gone" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := w.Write([]byte("body of " + r.URL.Path)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(files.Close)

	c, mb, _ := newEventChannel(t, config.SlackConfig{BotToken: "xoxb-secret"}, nil, nil)
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)

	c.handleMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "D1", Text: "see", TimeStamp: "3.0001", SubType: "file_share",
		Message: &slack.Msg{Files: []slack.File{
			{ID: "F1", Name: "a.txt", URLPrivateDownload: files.URL + "/a", URLPrivate: files.URL + "/wrong"},
			{ID: "F2", Name: "b.txt", URLPrivate: files.URL + "/b"},
			{ID: "F3", Name: "none.txt"},
			{ID: "F4", Name: "gone.txt", URLPrivate: files.URL + "/gone"},
		}},
	})
	msg := nextInbound(t, mb)
	if msg.Content != "see\n[file: a.txt]\n[file: b.txt]" {
		t.Errorf("content = %q", msg.Content)
	}
	if len(msg.Media) != 2 {
		t.Fatalf("media = %v, want two refs", msg.Media)
	}
	for i, want := range []string{"body of /a", "body of /b"} {
		p, err := store.Resolve(msg.Media[i])
		if err != nil {
			t.Fatalf("ref %q: %v", msg.Media[i], err)
		}
		if body, err := os.ReadFile(p); err != nil || string(body) != want {
			t.Errorf("file %d = %q, %v; want %q", i, body, err, want)
		}
	}
	mu.Lock()
	for _, a := range auth {
		if a != "Bearer xoxb-secret" {
			t.Errorf("download authorization = %q, want the bot token", a)
		}
	}
	mu.Unlock()

	// Without a media store the local path is passed on; a message that is
	// only an undownloadable file is dropped.
	c.SetMediaStore(nil)
	c.handleMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "D1", TimeStamp: "3.0002", SubType: "file_share",
		Message: &slack.Msg{Files: []slack.File{{ID: "F4", Name: "gone.txt", URLPrivate: files.URL + "/gone"}}},
	})
	c.handleMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "D1", TimeStamp: "3.0003", SubType: "file_share",
		Message: &slack.Msg{Files: []slack.File{{ID: "F1", Name: "a.txt", URLPrivate: files.URL + "/a"}}},
	})
	msg = nextInbound(t, mb)
	if msg.MessageID != "3.0003" || msg.Content != "\n[file: a.txt]" || len(msg.Media) != 1 {
		t.Fatalf("got %q %q %v, want 3.0003 with one local file", msg.MessageID, msg.Content, msg.Media)
	}
	if body, err := os.ReadFile(msg.Media[0]); err != nil || string(body) != "body of /a" {
		t.Errorf("local file = %q, %v", body, err)
	}
}

// An app mention reaches the agents without the mention, in the thread it
// starts (or continues), marked as a mention.
func TestHandleAppMention(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{}, []string{"U1"}, nil)

	c.handleAppMention(&slackevents.AppMentionEvent{User: "UBOT", Channel: "C1", Text: "<@UBOT> me", TimeStamp: "4.1"})
	c.handleAppMention(&slackevents.AppMentionEvent{User: "U2", Channel: "C1", Text: "<@UBOT> hi", TimeStamp: "4.2"})
	c.handleAppMention(&slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT>", TimeStamp: "4.3"})
	c.handleAppMention(&slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> new topic", TimeStamp: "4.4"})
	c.handleAppMention(&slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> more", TimeStamp: "4.5", ThreadTimeStamp: "4.0"})

	msg := nextInbound(t, mb)
	if msg.MessageID != "4.4" || msg.Content != "new topic" || msg.ChatID != "C1/4.4" {
		t.Fatalf("got %s %q in %q, want 4.4 %q in C1/4.4", msg.MessageID, msg.Content, msg.ChatID, "new topic")
	}
	if msg.Metadata["is_mention"] != "true" || msg.Metadata["team_id"] != "T1" || msg.Sender.DisplayName != "Alice" {
		t.Errorf("metadata %v sender %+v, want a named mention", msg.Metadata, msg.Sender)
	}
	msg = nextInbound(t, mb)
	if msg.MessageID != "4.5" || msg.ChatID != "C1/4.0" || msg.Metadata["thread_ts"] != "4.0" {
		t.Fatalf("got %s in %q, want 4.5 in the thread C1/4.0", msg.MessageID, msg.ChatID)
	}
	if _, ok := c.pendingAcks.Load("4.4"); !ok {
		t.Error("mention not waiting for its acknowledgement")
	}
}

// Slash command events that are malformed or from a sender not allowed
// publish nothing; an allowed one is acknowledged and published.
func TestHandleSlashCommand_Filtering(t *testing.T) {
	c, mb, _ := newEventChannel(t, config.SlackConfig{}, []string{"U1"}, nil)

	c.handleSlashCommand(socketmode.Event{Data: "not a command"})
	ev := slashEvent("/agency", "hello", "C1")
	cmd, ok := ev.Data.(slack.SlashCommand)
	if !ok {
		t.Fatal("slashEvent carries no command")
	}
	cmd.UserID = "U2"
	c.handleSlashCommand(socketmode.Event{Data: cmd, Request: &socketmode.Request{EnvelopeID: "e1"}})
	ev.Request = &socketmode.Request{EnvelopeID: "e2"}
	c.handleSlashCommand(ev)

	msg := nextInbound(t, mb)
	if msg.Sender.PlatformID != "U1" {
		t.Fatalf("published from %q, want only U1's command", msg.SenderID)
	}
	if msg.Content != "hello" {
		t.Errorf("content = %q, want hello", msg.Content)
	}
}

// Send posts to the channel, threads a reply under the message it answers,
// keeps a thread's replies in it, and refuses a bad chat or a stopped channel.
func TestSend_Threading(t *testing.T) {
	c, _, api := newEventChannel(t, config.SlackConfig{}, nil, nil)

	sends := []bus.OutboundMessage{
		{ChatID: "C1", Content: "top"},
		{ChatID: "C1", Content: "reply", ReplyToMessageID: "5.1"},
		{ChatID: "C1/5.0", Content: "in thread", ReplyToMessageID: "5.2"},
	}
	for _, m := range sends {
		if err := c.Send(context.Background(), m); err != nil {
			t.Fatalf("Send %q = %v", m.Content, err)
		}
	}
	posts := api.callsTo("chat.postMessage")
	if len(posts) != 3 {
		t.Fatalf("posts = %d, want 3", len(posts))
	}
	for i, wantTS := range []string{"", "5.1", "5.0"} {
		if got := posts[i].form.Get("thread_ts"); got != wantTS || posts[i].form.Get("channel") != "C1" {
			t.Errorf("post %d: channel %q thread_ts %q, want C1 and %q", i, posts[i].form.Get("channel"), got, wantTS)
		}
	}

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "/5.0", Content: "x"}); err == nil {
		t.Error("chat without a channel: nil error")
	}
	c.SetRunning(false)
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "C1", Content: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("stopped: %v, want ErrNotRunning", err)
	}
	if n := len(api.callsTo("chat.postMessage")); n != 3 {
		t.Errorf("posts = %d after the refusals, want 3", n)
	}
}

// A failed check mark is only logged; the acknowledgement is not retried.
func TestAcknowledge_ReactionFails(t *testing.T) {
	c, _, api := newEventChannel(t, config.SlackConfig{}, nil, map[string]string{
		"reactions.add": `{"ok":false,"error":"already_reacted"}`,
	})
	c.pendingAcks.Store("6.1", slackMessageRef{ChannelID: "C1", Timestamp: "6.1"})
	c.acknowledge("6.1")
	waitFor(t, "the check mark attempt", func() bool { return len(api.callsTo("reactions.add")) == 1 })
	c.acknowledge("6.1")
	if _, ok := c.pendingAcks.Load("6.1"); ok {
		t.Error("acknowledgement still pending")
	}
}

// ReactToMessage adds the eyes reaction and its undo removes it; a failed add
// is logged and still returns an undo; a chat without a channel does nothing.
func TestReactToMessage(t *testing.T) {
	c, _, api := newEventChannel(t, config.SlackConfig{}, nil, map[string]string{
		"reactions.remove": `{"ok":false,"error":"no_reaction"}`,
	})

	undo, err := c.ReactToMessage(context.Background(), "C1/7.0", "7.1")
	if err != nil {
		t.Fatalf("ReactToMessage = %v", err)
	}
	adds := api.callsTo("reactions.add")
	if len(adds) != 1 || adds[0].form.Get("name") != "eyes" || adds[0].form.Get("channel") != "C1" || adds[0].form.Get("timestamp") != "7.1" {
		t.Fatalf("reactions.add = %+v, want eyes on C1 7.1", adds)
	}
	undo()
	waitFor(t, "the reaction to be removed", func() bool { return len(api.callsTo("reactions.remove")) == 1 })
	if rm := api.callsTo("reactions.remove")[0]; rm.form.Get("name") != "eyes" || rm.form.Get("timestamp") != "7.1" {
		t.Errorf("reactions.remove = %+v", rm.form)
	}

	api.mu.Lock()
	api.replies["reactions.add"] = `{"ok":false,"error":"channel_not_found"}`
	api.mu.Unlock()
	undo, err = c.ReactToMessage(context.Background(), "C1", "7.2")
	if err != nil || undo == nil {
		t.Fatalf("failed add: undo nil %v, err %v; want an undo and no error", undo == nil, err)
	}

	// A slash command has no message to react to.
	c.rememberSlashResponse("trigger-1", "https://hooks.slack.invalid/1")
	undo, err = c.ReactToMessage(context.Background(), "C1", "trigger-1")
	if err != nil || undo == nil {
		t.Fatalf("slash command: undo nil %v, err %v; want a no-op undo", undo == nil, err)
	}
	undo()

	undo, err = c.ReactToMessage(context.Background(), "", "7.3")
	if err != nil || undo == nil {
		t.Fatalf("no channel: undo nil %v, err %v; want a no-op undo", undo == nil, err)
	}
	undo()
	if n := len(api.callsTo("reactions.add")); n != 2 {
		t.Errorf("reactions.add calls = %d, want 2", n)
	}
}

// A reply that names no slash command, or one never seen, has no response
// URL.
func TestSlashResponseURL_Unknown(t *testing.T) {
	c, _, _ := newEventChannel(t, config.SlackConfig{}, nil, nil)
	if _, ok := c.slashResponseURL(""); ok {
		t.Error("empty id has a response URL")
	}
	if _, ok := c.slashResponseURL("never-seen"); ok {
		t.Error("unknown id has a response URL")
	}
}
