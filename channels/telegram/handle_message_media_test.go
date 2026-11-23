package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/utils"
)

// fakeFile is one file the fake Bot API knows: getFile answers with path, and
// a download of path returns body (or 404 when missing is set).
type fakeFile struct {
	path    string
	body    string
	missing bool
}

// fileBot is a fake Bot API serving getFile and file downloads.
type fileBot struct {
	mu       sync.Mutex
	getFiles []string
}

func (f *fileBot) getFileCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getFiles...)
}

// newFileBotChannel returns a channel on a fake Bot API that knows files (by
// file_id), publishing to a fresh bus, with downloads staged in a temp dir.
// An unknown file_id is refused by getFile.
func newFileBotChannel(t *testing.T, files map[string]fakeFile) (*TelegramChannel, *bus.MessageBus, *fileBot) {
	t.Helper()
	utils.SetMediaStagingDir(t.TempDir(), 0, 0)
	t.Cleanup(func() { utils.SetMediaStagingDir("", 0, 0) })

	fb := &fileBot{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/file/bot") {
			for _, f := range files {
				if strings.HasSuffix(r.URL.Path, "/"+f.path) && !f.missing {
					writeBody(t, w, f.body)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			writeBody(t, w, getMeReply)
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			var req struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode getFile: %v", err)
			}
			fb.mu.Lock()
			fb.getFiles = append(fb.getFiles, req.FileID)
			fb.mu.Unlock()
			f, ok := files[req.FileID]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				writeBody(t, w, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
				return
			}
			writeBody(t, w, fmt.Sprintf(`{"ok":true,"result":{"file_id":%q,"file_unique_id":"u","file_path":%q}}`, req.FileID, f.path))
		default:
			writeBody(t, w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)

	mb := bus.NewMessageBus()
	ch, err := NewTelegramChannelFromConfig(config.TelegramBotConfig{
		Token: testToken, BaseURL: srv.URL, AllowFrom: []string{"*"},
	}, mb)
	require.NoError(t, err)
	ch.ctx = context.Background()
	return ch, mb, fb
}

// consume returns the next inbound message, failing the test if none comes.
func consume(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in, ok := mb.ConsumeInbound(ctx)
	require.True(t, ok, "expected an inbound message")
	return in
}

// requireNoInbound fails if a message is published.
func requireNoInbound(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	in, ok := mb.ConsumeInbound(ctx)
	require.False(t, ok, "unexpected inbound message %+v", in)
}

func privateMessage(id int) *telego.Message {
	return &telego.Message{
		MessageID: id,
		Chat:      telego.Chat{ID: 12345, Type: "private"},
		From:      &telego.User{ID: 7, FirstName: "Alice", Username: "alice"},
	}
}

var allMediaFiles = map[string]fakeFile{
	"photo-big": {path: "photos/file_1.jpg", body: "jpeg-bytes"},
	"voice":     {path: "voice/file_3.oga", body: "oga-bytes"},
	"audio":     {path: "music/file_4", body: "mp3-bytes"},
	"doc":       {path: "documents/report.pdf", body: "pdf-bytes"},
}

// Each attachment is downloaded, registered with the media store, and named in
// the content after the text and caption; the largest photo size is the one
// fetched, and a path without an extension gets the type's.
func TestHandleMessage_MediaStoredAndNamed(t *testing.T) {
	ch, mb, fb := newFileBotChannel(t, allMediaFiles)
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)

	m := privateMessage(1)
	m.Text = "look"
	m.Caption = "at this"
	m.Photo = []telego.PhotoSize{{FileID: "photo-small"}, {FileID: "photo-big"}}
	m.Voice = &telego.Voice{FileID: "voice"}
	m.Audio = &telego.Audio{FileID: "audio"}
	m.Document = &telego.Document{FileID: "doc"}

	require.NoError(t, ch.handleMessage(context.Background(), m))
	in := consume(t, mb)

	assert.Equal(t, "look\nat this\n[image: photo]\n[voice]\n[audio]\n[file]", in.Content)
	assert.Equal(t, []string{"photo-big", "voice", "audio", "doc"}, fb.getFileCalls())
	require.Len(t, in.Media, 4)

	wantBody := []string{"jpeg-bytes", "oga-bytes", "mp3-bytes", "pdf-bytes"}
	wantName := []string{"photo.jpg", "voice.ogg", "audio.mp3", "document"}
	wantExt := []string{".jpg", ".oga", ".mp3", ".pdf"}
	for i, ref := range in.Media {
		require.True(t, strings.HasPrefix(ref, "media://"), "media %d is a store ref: %s", i, ref)
		path, meta, err := store.ResolveWithMeta(ref)
		require.NoError(t, err)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, wantBody[i], string(got))
		assert.Equal(t, wantName[i], meta.Filename)
		assert.Equal(t, "telegram", meta.Source)
		assert.Equal(t, wantExt[i], filepath.Ext(path))
	}
}

// Without a media store the downloaded file's local path is passed on.
func TestHandleMessage_MediaWithoutStoreUsesLocalPath(t *testing.T) {
	ch, mb, _ := newFileBotChannel(t, allMediaFiles)
	m := privateMessage(1)
	m.Document = &telego.Document{FileID: "doc"}

	require.NoError(t, ch.handleMessage(context.Background(), m))
	in := consume(t, mb)

	assert.Equal(t, "[file]", in.Content)
	require.Len(t, in.Media, 1)
	got, err := os.ReadFile(in.Media[0])
	require.NoError(t, err)
	assert.Equal(t, "pdf-bytes", string(got))
	assert.Equal(t, utils.MediaTempDir(), filepath.Dir(in.Media[0]))
}

// Each kind of attachment alone is named without a leading newline.
func TestHandleMessage_SingleAttachmentContent(t *testing.T) {
	tests := []struct {
		name string
		set  func(*telego.Message)
		want string
	}{
		{"photo", func(m *telego.Message) { m.Photo = []telego.PhotoSize{{FileID: "photo-big"}} }, "[image: photo]"},
		{"voice", func(m *telego.Message) { m.Voice = &telego.Voice{FileID: "voice"} }, "[voice]"},
		{"audio", func(m *telego.Message) { m.Audio = &telego.Audio{FileID: "audio"} }, "[audio]"},
		{"document", func(m *telego.Message) { m.Document = &telego.Document{FileID: "doc"} }, "[file]"},
		{"caption only", func(m *telego.Message) { m.Caption = "just a caption" }, "just a caption"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, mb, _ := newFileBotChannel(t, allMediaFiles)
			m := privateMessage(1)
			tt.set(m)
			require.NoError(t, ch.handleMessage(context.Background(), m))
			in := consume(t, mb)
			assert.Equal(t, tt.want, in.Content)
		})
	}
}

// An attachment that cannot be fetched (getFile refused, no file path, or the
// download failing) is left out; a message with nothing else is
// "[empty message]".
func TestHandleMessage_FailedDownloadsLeftOut(t *testing.T) {
	files := map[string]fakeFile{
		"no-path": {path: ""},
		"gone":    {path: "music/file_9.mp3", missing: true},
		"gone-doc": {
			path: "documents/file_9.pdf", missing: true,
		},
	}
	ch, mb, fb := newFileBotChannel(t, files)
	m := privateMessage(1)
	m.Photo = []telego.PhotoSize{{FileID: "unknown"}}
	m.Voice = &telego.Voice{FileID: "no-path"}
	m.Audio = &telego.Audio{FileID: "gone"}
	m.Document = &telego.Document{FileID: "gone-doc"}

	require.NoError(t, ch.handleMessage(context.Background(), m))
	in := consume(t, mb)

	assert.Equal(t, "[empty message]", in.Content)
	assert.Empty(t, in.Media)
	assert.Equal(t, []string{"unknown", "no-path", "gone", "gone-doc"}, fb.getFileCalls())
}

// A voice message whose getFile is refused is left out as well.
func TestHandleMessage_VoiceGetFileRefused(t *testing.T) {
	ch, mb, _ := newFileBotChannel(t, nil)
	m := privateMessage(1)
	m.Text = "hi"
	m.Voice = &telego.Voice{FileID: "unknown"}

	require.NoError(t, ch.handleMessage(context.Background(), m))
	in := consume(t, mb)
	assert.Equal(t, "hi", in.Content)
	assert.Empty(t, in.Media)
}

// A message without a body or a sender is an error; one from a sender not on
// the allow list is dropped before any attachment is fetched.
func TestHandleMessage_RejectsBeforeDownloading(t *testing.T) {
	ch, mb, fb := newFileBotChannel(t, allMediaFiles)

	require.EqualError(t, ch.handleMessage(context.Background(), nil), "message is nil")

	noSender := privateMessage(1)
	noSender.From = nil
	require.EqualError(t, ch.handleMessage(context.Background(), noSender), "message sender (user) is nil")

	ch.BaseChannel = channels.NewBaseChannel("telegram", nil, mb, []string{"999"})
	m := privateMessage(2)
	m.Document = &telego.Document{FileID: "doc"}
	require.NoError(t, ch.handleMessage(context.Background(), m))

	requireNoInbound(t, mb)
	assert.Empty(t, fb.getFileCalls())
	assert.Empty(t, ch.chatIDs)
}

// With message content logging on, the received-message log carries a
// preview of the text.
func TestHandleMessage_LogsPreviewWhenContentLoggingOn(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.RedirectForTest(&buf)
	t.Cleanup(restore)
	prev := logger.GetLogMessageContent()
	logger.SetLogMessageContent(true)
	t.Cleanup(func() { logger.SetLogMessageContent(prev) })

	ch, mb, _ := newFileBotChannel(t, nil)
	m := privateMessage(1)
	m.Text = "preview me"
	require.NoError(t, ch.handleMessage(context.Background(), m))
	consume(t, mb)

	assert.Contains(t, buf.String(), `"preview":"preview me"`)
}

// With coalescing on, plain text waits in the buffer; a command flushes the
// sender's buffered text first and is itself dispatched at once.
func TestHandleMessage_CommandFlushesCoalescedText(t *testing.T) {
	ch, mb, _ := newFileBotChannel(t, nil)
	ch.coalescer = newMessageCoalescer(config.CoalesceConfig{WindowMS: int(time.Hour / time.Millisecond), MaxWaitMS: int(time.Hour / time.Millisecond)}, ch.dispatchCoalesced)

	first := privateMessage(1)
	first.Text = "part one"
	second := privateMessage(2)
	second.Text = "part two"
	require.NoError(t, ch.handleMessage(context.Background(), first))
	require.NoError(t, ch.handleMessage(context.Background(), second))
	requireNoInbound(t, mb)

	cmd := privateMessage(3)
	cmd.Text = "/help"
	require.NoError(t, ch.handleMessage(context.Background(), cmd))

	in := consume(t, mb)
	assert.Equal(t, "part one\npart two", in.Content)
	assert.Equal(t, "2", in.MessageID)
	in = consume(t, mb)
	assert.Equal(t, "/help", in.Content)
	assert.Equal(t, "3", in.MessageID)
}
