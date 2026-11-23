package telegram

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
)

// roundTripperFunc is an http.RoundTripper that is not an *http.Transport.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// swapDefaultTransport replaces http.DefaultTransport for the test.
func swapDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// A configured proxy is used for every request.
func TestNewHTTPTransport_Proxy(t *testing.T) {
	tr, err := newHTTPTransport("http://proxy.example:3128")
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.org/", nil)
	require.NoError(t, err)
	u, err := tr.Proxy(req)
	require.NoError(t, err)
	assert.Equal(t, "http://proxy.example:3128", u.String())
}

// A proxy that is not a URL is refused, and so is the bot.
func TestNewHTTPTransport_InvalidProxy(t *testing.T) {
	_, err := newHTTPTransport("http://[::1")
	require.ErrorContains(t, err, "invalid proxy URL")

	_, err = NewTelegramChannelFromConfig(config.TelegramBotConfig{Token: testToken, Proxy: "http://[::1"}, bus.NewMessageBus())
	require.ErrorContains(t, err, "invalid proxy URL")
}

// A default transport without a TLS config still gets one pinned to HTTP/1.1.
// (A non-nil TLSNextProto turns off HTTP/2 setup, which would otherwise add a
// TLS config when the transport is cloned.)
func TestNewHTTPTransport_NoDefaultTLSConfig(t *testing.T) {
	swapDefaultTransport(t, &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}})
	tr, err := newHTTPTransport("")
	require.NoError(t, err)
	require.NotNil(t, tr.TLSClientConfig)
	assert.Equal(t, []string{"http/1.1"}, tr.TLSClientConfig.NextProtos)
}

// A default transport that is not an *http.Transport cannot be cloned.
func TestNewHTTPTransport_ForeignDefaultTransport(t *testing.T) {
	swapDefaultTransport(t, roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, io.EOF }))
	_, err := newHTTPTransport("")
	require.Error(t, err)

	_, err = NewTelegramChannelFromConfig(config.TelegramBotConfig{Token: testToken}, bus.NewMessageBus())
	require.Error(t, err)
}

// A bot needs a well-formed token.
func TestNewTelegramChannelFromConfig_Token(t *testing.T) {
	_, err := NewTelegramChannelFromConfig(config.TelegramBotConfig{}, bus.NewMessageBus())
	require.EqualError(t, err, "telegram bot token is required")

	_, err = NewTelegramChannelFromConfig(config.TelegramBotConfig{Token: "not-a-token"}, bus.NewMessageBus())
	require.ErrorContains(t, err, "failed to create telegram bot")
}

// updatesBot is a fake Bot API whose first getUpdates delivers updates, after
// which getUpdates holds until the request is cancelled.
func updatesBot(t *testing.T, updates string) *httptest.Server {
	t.Helper()
	var delivered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			writeBody(t, w, getMeReply)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if delivered.CompareAndSwap(false, true) {
				writeBody(t, w, `{"ok":true,"result":`+updates+`}`)
				return
			}
			<-r.Context().Done()
		default:
			writeBody(t, w, okTrue)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A started bot hands polled messages to the bus; with coalescing off each
// is published at once.
func TestStart_DeliversPolledMessage(t *testing.T) {
	srv := updatesBot(t, `[{"update_id":1,"message":{"message_id":3,"date":0,`+
		`"chat":{"id":12345,"type":"private"},"from":{"id":7,"is_bot":false,"first_name":"Alice"},"text":"hello"}}]`)
	off := false
	mb := bus.NewMessageBus()
	ch, err := NewTelegramChannelFromConfig(config.TelegramBotConfig{
		Token: testToken, BaseURL: srv.URL, AllowFrom: []string{"*"},
		Coalesce: config.CoalesceConfig{Enabled: &off},
	}, mb)
	require.NoError(t, err)
	ch.registerFunc = func(context.Context, []commands.Definition) error { return nil }

	require.NoError(t, ch.Start(context.Background()))
	t.Cleanup(func() {
		if err := ch.Stop(context.Background()); err != nil {
			t.Errorf("Stop = %v", err)
		}
	})
	assert.Nil(t, ch.coalescer)

	in := consume(t, mb)
	assert.Equal(t, "hello", in.Content)
	assert.Equal(t, "12345", in.ChatID)
	assert.Equal(t, "3", in.MessageID)
}

// Stop with a context that has already ended returns its error rather than
// waiting for the bot's goroutines.
func TestStop_EndedContext(t *testing.T) {
	srv := updatesBot(t, `[]`)
	ch, err := NewTelegramChannelFromConfig(config.TelegramBotConfig{Token: testToken, BaseURL: srv.URL}, bus.NewMessageBus())
	require.NoError(t, err)
	ch.registerFunc = func(context.Context, []commands.Definition) error { return nil }
	require.NoError(t, ch.Start(context.Background()))
	require.Eventually(t, ch.bh.IsRunning, 5*time.Second, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, ch.Stop(ctx), context.Canceled)
	assert.False(t, ch.IsRunning())
	select {
	case <-ch.pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not exit")
	}
}

func TestIsBotMentioned_Entities(t *testing.T) {
	tests := []struct {
		name string
		msg  *telego.Message
		want bool
	}{
		{
			name: "entity past the text is skipped",
			msg: &telego.Message{Text: "@testbot", Entities: []telego.MessageEntity{
				{Type: telego.EntityTypeMention, Offset: 20, Length: 8},
				{Type: telego.EntityTypeMention, Offset: -1, Length: 8},
				{Type: telego.EntityTypeMention, Offset: 0, Length: 0},
			}},
			want: false,
		},
		{
			name: "text mention of this bot",
			msg: &telego.Message{Text: "hey bot", Entities: []telego.MessageEntity{
				{Type: telego.EntityTypeTextMention, Offset: 4, Length: 3, User: &telego.User{Username: "TestBot"}},
			}},
			want: true,
		},
		{
			name: "text mention of someone else",
			msg: &telego.Message{Text: "hey bob", Entities: []telego.MessageEntity{
				{Type: telego.EntityTypeTextMention, Offset: 4, Length: 3, User: &telego.User{Username: "bob"}},
			}},
			want: false,
		},
		{
			name: "mention in a caption",
			msg: &telego.Message{Caption: "@testbot look", CaptionEntities: []telego.MessageEntity{
				{Type: telego.EntityTypeMention, Offset: 0, Length: 8},
			}},
			want: true,
		},
		{
			name: "no entities",
			msg:  &telego.Message{Text: "@testbot"},
			want: false,
		},
	}
	ch, _ := newGroupMentionOnlyChannel(t, "testbot")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ch.isBotMentioned(tt.msg))
		})
	}
}

func TestIsBotCommandEntityForThisBot(t *testing.T) {
	tests := []struct {
		entity, bot string
		want        bool
	}{
		{"help", "testbot", false},
		{"/", "testbot", false},
		{"/help", "testbot", true},
		{"/help@", "testbot", false},
		{"/help@testbot", "", false},
		{"/help@TestBot", "testbot", true},
		{"/help@otherbot", "testbot", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isBotCommandEntityForThisBot(tt.entity, tt.bot), "%q for %q", tt.entity, tt.bot)
	}
}

// Without a bot username there is nothing to strip.
func TestStripBotMention_NoUsername(t *testing.T) {
	ch, _ := newGroupMentionOnlyChannel(t, "")
	assert.Equal(t, " @testbot hi ", ch.stripBotMention(" @testbot hi "))

	ch, _ = newGroupMentionOnlyChannel(t, "testbot")
	assert.Equal(t, "hi", ch.stripBotMention(" @TestBot hi "))
}
