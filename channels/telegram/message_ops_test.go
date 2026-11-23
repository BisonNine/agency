package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
)

// apiCall is one request the fake Bot API received.
type apiCall struct {
	method string
	body   map[string]any
}

// recordingBot is a fake Bot API that records every call but getMe and
// answers with reply (status, JSON body).
type recordingBot struct {
	mu    sync.Mutex
	calls []apiCall
	seen  chan string
}

func (b *recordingBot) all() []apiCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]apiCall(nil), b.calls...)
}

func (b *recordingBot) count(method string) int {
	n := 0
	for _, c := range b.all() {
		if c.method == method {
			n++
		}
	}
	return n
}

const okTrue = `{"ok":true,"result":true}`

func newRecordingBot(t *testing.T, reply func(method string) (int, string)) (*TelegramChannel, *recordingBot) {
	t.Helper()
	rb := &recordingBot{seen: make(chan string, 1000)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if method == "getMe" {
			writeBody(t, w, getMeReply)
			return
		}
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s: %v", method, err)
		}
		rb.mu.Lock()
		rb.calls = append(rb.calls, apiCall{method: method, body: body})
		rb.mu.Unlock()
		select {
		case rb.seen <- method:
		default:
		}
		status, out := reply(method)
		w.WriteHeader(status)
		writeBody(t, w, out)
	}))
	t.Cleanup(srv.Close)
	return newFakeBotChannel(t, srv), rb
}

func alwaysOK(string) (int, string) { return http.StatusOK, okTrue }

const badRequest = `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`

func alwaysBad(string) (int, string) { return http.StatusBadRequest, badRequest }

// StartTyping shows typing in the chat's topic at once and repeats it until
// stopped.
func TestStartTyping_RepeatsUntilStopped(t *testing.T) {
	prev := typingInterval
	typingInterval = 5 * time.Millisecond
	t.Cleanup(func() { typingInterval = prev })

	ch, rb := newRecordingBot(t, alwaysOK)
	stop, err := ch.StartTyping(context.Background(), "-100123/7")
	require.NoError(t, err)

	calls := rb.all()
	require.NotEmpty(t, calls, "the first typing action is sent before StartTyping returns")
	assert.Equal(t, "sendChatAction", calls[0].method)
	assert.InDelta(t, -100123, calls[0].body["chat_id"], 0)
	assert.InDelta(t, 7, calls[0].body["message_thread_id"], 0)
	assert.Equal(t, telego.ChatActionTyping, calls[0].body["action"])

	require.Eventually(t, func() bool { return rb.count("sendChatAction") >= 3 }, 5*time.Second, time.Millisecond)
	stop()
	stop() // idempotent

	// At most a request already in flight lands after stop.
	n := rb.count("sendChatAction")
	deadline := time.After(20 * typingInterval)
	for {
		select {
		case <-rb.seen:
			continue
		case <-deadline:
		}
		break
	}
	assert.LessOrEqual(t, rb.count("sendChatAction"), n+1)
}

// A refused typing action is not an error: typing is best effort.
func TestStartTyping_FailuresAreNotErrors(t *testing.T) {
	prev := typingInterval
	typingInterval = 5 * time.Millisecond
	t.Cleanup(func() { typingInterval = prev })

	ch, rb := newRecordingBot(t, alwaysBad)
	stop, err := ch.StartTyping(context.Background(), "12345")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rb.count("sendChatAction") >= 2 }, 5*time.Second, time.Millisecond)
	stop()
}

// An invalid chat ID is refused without a call; the stop function is usable.
func TestStartTyping_InvalidChatID(t *testing.T) {
	ch, rb := newRecordingBot(t, alwaysOK)
	stop, err := ch.StartTyping(context.Background(), "not-a-chat")
	require.Error(t, err)
	require.NotNil(t, stop)
	stop()
	assert.Empty(t, rb.all())
}

// EditMessage replaces the text with its HTML rendering.
func TestEditMessage(t *testing.T) {
	ch, rb := newRecordingBot(t, func(string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":{"message_id":5,"date":0,"chat":{"id":12345,"type":"private"}}}`
	})
	require.NoError(t, ch.EditMessage(context.Background(), "12345/9", "5", "**done**"))

	calls := rb.all()
	require.Len(t, calls, 1)
	assert.Equal(t, "editMessageText", calls[0].method)
	assert.InDelta(t, 12345, calls[0].body["chat_id"], 0)
	assert.InDelta(t, 5, calls[0].body["message_id"], 0)
	assert.Equal(t, "<b>done</b>", calls[0].body["text"])
	assert.Equal(t, telego.ModeHTML, calls[0].body["parse_mode"])
}

// EditMessage refuses a bad chat or message ID without a call, and returns
// Telegram's refusal.
func TestEditMessage_Errors(t *testing.T) {
	ch, rb := newRecordingBot(t, alwaysBad)
	require.Error(t, ch.EditMessage(context.Background(), "x", "5", "a"))
	require.Error(t, ch.EditMessage(context.Background(), "12345", "five", "a"))
	assert.Empty(t, rb.all())

	err := ch.EditMessage(context.Background(), "12345", "5", "a")
	require.ErrorContains(t, err, "message to edit not found")
}

// DeleteMessage deletes the message in the chat.
func TestDeleteMessage(t *testing.T) {
	ch, rb := newRecordingBot(t, alwaysOK)
	require.NoError(t, ch.DeleteMessage(context.Background(), "-100123/4", "77"))

	calls := rb.all()
	require.Len(t, calls, 1)
	assert.Equal(t, "deleteMessage", calls[0].method)
	assert.InDelta(t, -100123, calls[0].body["chat_id"], 0)
	assert.InDelta(t, 77, calls[0].body["message_id"], 0)
}

// DeleteMessage refuses a bad chat or message ID without a call, and returns
// Telegram's refusal.
func TestDeleteMessage_Errors(t *testing.T) {
	ch, rb := newRecordingBot(t, alwaysBad)
	require.Error(t, ch.DeleteMessage(context.Background(), "x", "5"))
	require.Error(t, ch.DeleteMessage(context.Background(), "12345", "five"))
	assert.Empty(t, rb.all())

	require.ErrorContains(t, ch.DeleteMessage(context.Background(), "12345", "5"), "message to edit not found")
}

// SendPlaceholder sends the configured text (or the default) to the chat's
// topic and returns the new message's ID; it does nothing when disabled.
func TestSendPlaceholder(t *testing.T) {
	reply := func(string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":{"message_id":42,"date":0,"chat":{"id":12345,"type":"private"}}}`
	}
	tests := []struct {
		name     string
		cfg      config.PlaceholderConfig
		chatID   string
		wantID   string
		wantText string
		wantErr  bool
	}{
		{name: "disabled", cfg: config.PlaceholderConfig{}, chatID: "12345"},
		{name: "default text", cfg: config.PlaceholderConfig{Enabled: true}, chatID: "12345/3", wantID: "42", wantText: "Thinking... 💭"},
		{name: "configured text", cfg: config.PlaceholderConfig{Enabled: true, Text: "Working"}, chatID: "12345", wantID: "42", wantText: "Working"},
		{name: "invalid chat", cfg: config.PlaceholderConfig{Enabled: true}, chatID: "x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, rb := newRecordingBot(t, reply)
			ch.placeholderCfg = tt.cfg
			id, err := ch.SendPlaceholder(context.Background(), tt.chatID)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, rb.all())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, id)
			calls := rb.all()
			if tt.wantText == "" {
				assert.Empty(t, calls)
				return
			}
			require.Len(t, calls, 1)
			assert.Equal(t, "sendMessage", calls[0].method)
			assert.Equal(t, tt.wantText, calls[0].body["text"])
			if strings.Contains(tt.chatID, "/") {
				assert.InDelta(t, 3, calls[0].body["message_thread_id"], 0)
			}
		})
	}
}

// RegisterCommands sets the bot's commands, skipping definitions without a
// name or description, and does not set them when Telegram already has them.
func TestRegisterCommands(t *testing.T) {
	defs := []commands.Definition{
		{Name: "help", Description: "Show help"},
		{Name: "", Description: "no name"},
		{Name: "nodesc"},
		{Name: "new", Description: "New session"},
	}
	const want = `[{"command":"help","description":"Show help"},{"command":"new","description":"New session"}]`
	tests := []struct {
		name      string
		current   func() (int, string)
		setStatus int
		wantSet   bool
		wantErr   bool
	}{
		{
			name:    "up to date",
			current: func() (int, string) { return http.StatusOK, `{"ok":true,"result":` + want + `}` },
		},
		{
			name:      "different",
			current:   func() (int, string) { return http.StatusOK, `{"ok":true,"result":[]}` },
			setStatus: http.StatusOK,
			wantSet:   true,
		},
		{
			name: "current unreadable",
			current: func() (int, string) {
				return http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal"}`
			},
			setStatus: http.StatusOK,
			wantSet:   true,
		},
		{
			name:      "set refused",
			current:   func() (int, string) { return http.StatusOK, `{"ok":true,"result":[]}` },
			setStatus: http.StatusBadRequest,
			wantSet:   true,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, rb := newRecordingBot(t, func(method string) (int, string) {
				if method == "getMyCommands" {
					return tt.current()
				}
				if tt.setStatus != http.StatusOK {
					return tt.setStatus, `{"ok":false,"error_code":400,"description":"Bad Request: bad command"}`
				}
				return http.StatusOK, okTrue
			})
			err := ch.RegisterCommands(context.Background(), defs)
			if tt.wantErr {
				require.ErrorContains(t, err, "bad command")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, boolToInt(tt.wantSet), rb.count("setMyCommands"))
			if tt.wantSet {
				calls := rb.all()
				got, mErr := json.Marshal(calls[len(calls)-1].body["commands"])
				require.NoError(t, mErr)
				assert.JSONEq(t, want, string(got))
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// An empty backoff schedule retries without delay.
func TestCommandRegistrationDelay_EmptyBackoff(t *testing.T) {
	assert.Zero(t, commandRegistrationDelay(nil, 3))
}

// With no commands to register no registration runs.
func TestStartCommandRegistration_NoDefinitions(t *testing.T) {
	ch := &TelegramChannel{registerFunc: func(context.Context, []commands.Definition) error {
		t.Error("register must not be called")
		return nil
	}}
	ch.startCommandRegistration(context.Background(), nil)
	assert.Nil(t, ch.commandRegDone)
	assert.Nil(t, ch.commandRegCancel)
}

// combineMessages of nothing is nothing.
func TestCombineMessages_Empty(t *testing.T) {
	assert.Nil(t, combineMessages(nil))
}

// A chat ID whose chat part is not a number is refused.
func TestParseTelegramChatID_InvalidChatBeforeThread(t *testing.T) {
	_, _, err := parseTelegramChatID("abc/5")
	require.Error(t, err)
}

func TestMarkdownToTelegramHTML_Empty(t *testing.T) {
	assert.Empty(t, markdownToTelegramHTML(""))
}

// pollUpdates returns, closing updates, when cancelled while nobody reads
// the update it is handing on.
func TestPollUpdates_StopsWhileHandingOn(t *testing.T) {
	ch, _ := newRecordingBot(t, func(string) (int, string) {
		return http.StatusOK, `{"ok":true,"result":[{"update_id":1},{"update_id":2}]}`
	})
	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan telego.Update) // unbuffered: the loop waits on each hand-on
	done := make(chan struct{})
	go func() {
		defer close(done)
		ch.pollUpdates(ctx, updates)
	}()
	select {
	case u := <-updates:
		require.Equal(t, 1, u.UpdateID)
	case <-time.After(5 * time.Second):
		t.Fatal("no update handed on")
	}
	// The loop now waits to hand on update 2, which nobody reads.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollUpdates did not return after cancel")
	}
	_, open := <-updates
	assert.False(t, open)
}
