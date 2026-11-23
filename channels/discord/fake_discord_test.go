package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
)

// fakeCall is one write the fake Discord API received.
type fakeCall struct {
	method    string
	channelID string
	messageID string
	content   string
	replyTo   string
}

// fakeDiscord is a stand-in for the Discord REST API: it serves channels and
// messages from its maps, records every message posted or edited and every
// typing indicator, and can be told to fail or hang the writes.
type fakeDiscord struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	calls       []fakeCall
	channels    map[string]string             // channel id → name
	messages    map[string]*discordgo.Message // "channel/message" → message
	writeStatus int                           // non-zero: posts and edits answer this
	typingFail  bool                          // typing answers 500
	userFail    bool                          // the bot-user lookup answers 401
	hang        chan struct{}                 // non-nil: posts wait for it to close

	typing  chan string   // channel ids typed in
	arrived chan fakeCall // every post or edit, as it arrives
}

// newFakeDiscord starts the fake and points discordgo's channel and user
// endpoints at it for the test.
func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{
		t:        t,
		channels: map[string]string{},
		messages: map[string]*discordgo.Message{},
		typing:   make(chan string, 64),
		arrived:  make(chan fakeCall, 64),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	origChannels, origUsers := discordgo.EndpointChannels, discordgo.EndpointUsers
	discordgo.EndpointChannels = f.srv.URL + "/channels/"
	discordgo.EndpointUsers = f.srv.URL + "/users/"
	t.Cleanup(func() {
		discordgo.EndpointChannels, discordgo.EndpointUsers = origChannels, origUsers
	})
	return f
}

// hangWrites makes posts block until the test ends.
func (f *fakeDiscord) hangWrites() {
	f.mu.Lock()
	f.hang = make(chan struct{})
	h := f.hang
	f.mu.Unlock()
	// Registered after srv.Close, so it runs first and frees the handlers.
	f.t.Cleanup(func() { close(h) })
}

func (f *fakeDiscord) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeDiscord) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("write reply: %v", err)
	}
}

func (f *fakeDiscord) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
	}
	if r.URL.Path == "/users/@me" {
		f.mu.Lock()
		fail := f.userFail
		f.mu.Unlock()
		if fail {
			f.reply(w, http.StatusUnauthorized, map[string]any{"code": 0, "message": "401: Unauthorized"})
			return
		}
		f.reply(w, http.StatusOK, map[string]string{"id": "bot", "username": "botty"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/channels/"), "/")
	f.mu.Lock()
	status, typingFail, hang := f.writeStatus, f.typingFail, f.hang
	f.mu.Unlock()
	notFound := map[string]any{"code": discordgo.ErrCodeUnknownChannel, "message": "Unknown Channel"}

	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		f.mu.Lock()
		name, ok := f.channels[parts[0]]
		f.mu.Unlock()
		if !ok {
			f.reply(w, http.StatusNotFound, notFound)
			return
		}
		f.reply(w, http.StatusOK, map[string]string{"id": parts[0], "name": name})
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "messages":
		f.mu.Lock()
		msg, ok := f.messages[parts[0]+"/"+parts[2]]
		f.mu.Unlock()
		if !ok {
			f.reply(w, http.StatusNotFound, map[string]any{"code": discordgo.ErrCodeUnknownMessage, "message": "Unknown Message"})
			return
		}
		f.reply(w, http.StatusOK, msg)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "typing":
		f.typing <- parts[0]
		if typingFail {
			f.reply(w, http.StatusInternalServerError, map[string]any{"code": 0, "message": "boom"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case (r.Method == http.MethodPost && len(parts) == 2) || (r.Method == http.MethodPatch && len(parts) == 3):
		var sent struct {
			Content   string `json:"content"`
			Reference *struct {
				MessageID string `json:"message_id"`
			} `json:"message_reference"`
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(body, &sent); err != nil {
				f.t.Errorf("decode %s: %v", r.URL.Path, err)
			}
		}
		call := fakeCall{method: r.Method, channelID: parts[0], content: sent.Content}
		if len(parts) == 3 {
			call.messageID = parts[2]
		}
		if sent.Reference != nil {
			call.replyTo = sent.Reference.MessageID
		}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		f.arrived <- call
		if hang != nil {
			<-hang
		}
		if status != 0 {
			f.reply(w, status, notFound)
			return
		}
		f.reply(w, http.StatusOK, map[string]string{"id": "posted-1", "channel_id": parts[0]})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newFakeChannel is a running channel on a session that talks to the fake,
// publishing inbound messages to the returned bus.
func newFakeChannel(t *testing.T, cfg config.DiscordConfig) (*DiscordChannel, *bus.MessageBus) {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	session.MaxRestRetries = 0
	session.State.User = &discordgo.User{ID: "bot"}
	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)
	allow := []string(cfg.AllowFrom)
	if allow == nil {
		allow = []string{"*"}
	}
	c := &DiscordChannel{
		BaseChannel: channels.NewBaseChannel("discord", cfg, mb, allow,
			channels.WithGroupTrigger(cfg.GroupTrigger)),
		session:    session,
		config:     cfg,
		typingStop: make(map[string]chan struct{}),
		botUserID:  "bot",
	}
	c.ctx, c.cancel = t.Context(), func() {}
	c.SetRunning(true)
	return c, mb
}
