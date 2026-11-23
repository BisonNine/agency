package matrix

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
)

// recordingStore is a media store that records the files registered with it.
// With fail set it refuses every file, so the channel hands on the path.
type recordingStore struct {
	media.MediaStore
	fail bool

	mu    sync.Mutex
	paths []string
}

func (s *recordingStore) Store(path string, meta media.MediaMeta, scope string) (string, error) {
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	if s.fail {
		return "", errors.New("store full")
	}
	return s.MediaStore.Store(path, meta, scope)
}

func (s *recordingStore) stored() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func imageMsg() *event.MessageEventContent {
	return &event.MessageEventContent{
		MsgType: event.MsgImage, Body: "cat.png", URL: "mxc://matrix.test/abc",
		Info: &event.FileInfo{MimeType: "image/png"},
	}
}

// downloads counts the media downloads the homeserver has received so far.
func (hs *stubHomeserver) downloads() int {
	n := 0
	for {
		select {
		case r := <-hs.reqs:
			if strings.Contains(r.Path, "/download/") {
				n++
			}
		default:
			return n
		}
	}
}

// A media message the channel does not hand on is never downloaded: a sender
// not on the allow list, or a group message the trigger rules ignore.
func TestHandleMessageEvent_IgnoredMediaNotDownloaded(t *testing.T) {
	tests := []struct {
		name    string
		members int
		edit    func(*config.MatrixConfig)
		sender  id.UserID
		caption string
	}{
		{
			name:    "sender not allowed",
			members: 2,
			edit: func(cfg *config.MatrixConfig) {
				cfg.AllowFrom = config.FlexibleStringSlice{"@bob:matrix.test"}
			},
			sender: "@alice:matrix.test",
		},
		{
			name:    "group message without mention",
			members: 3,
			edit: func(cfg *config.MatrixConfig) {
				cfg.GroupTrigger = config.GroupTriggerConfig{MentionOnly: true}
			},
			sender: "@alice:matrix.test",
		},
		{
			name:    "group message without prefix",
			members: 3,
			edit: func(cfg *config.MatrixConfig) {
				cfg.GroupTrigger = config.GroupTriggerConfig{Prefixes: []string{"!ask"}}
			},
			sender:  "@alice:matrix.test",
			caption: "look at this",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newStubHomeserver(t)
			hs.members = tt.members
			hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, "PNGDATA") }
			mb := bus.NewMessageBus()
			defer mb.Close()
			c := stubChannel(t, hs, mb, tt.edit)
			store := &recordingStore{MediaStore: media.NewFileMediaStore()}
			c.SetMediaStore(store)

			msg := imageMsg()
			if tt.caption != "" {
				msg.Body = tt.caption
				msg.FileName = "cat.png"
			}
			evt := msgEvent(msg)
			evt.Sender = tt.sender
			c.handleMessageEvent(context.Background(), evt)

			if got := drainInbound(t, mb); len(got) != 0 {
				t.Fatalf("delivered %q, want nothing", got)
			}
			if n := hs.downloads(); n != 0 {
				t.Fatalf("downloads = %d, want 0", n)
			}
			if got := store.stored(); len(got) != 0 {
				t.Fatalf("stored %q, want nothing", got)
			}
		})
	}
}

// A group message the trigger rules accept is still downloaded and handed on
// with its media, the prefix decided on the caption and marker alone.
func TestHandleMessageEvent_TriggeredGroupMediaDownloaded(t *testing.T) {
	hs := newStubHomeserver(t)
	hs.members = 3
	hs.download = func(w http.ResponseWriter, _ *http.Request) { reply(t, w, "PNGDATA") }
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := stubChannel(t, hs, mb, func(cfg *config.MatrixConfig) {
		cfg.GroupTrigger = config.GroupTriggerConfig{Prefixes: []string{"!ask"}}
	})
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)

	msg := imageMsg()
	msg.Body = "!ask what is this"
	msg.FileName = "cat.png"
	c.handleMessageEvent(context.Background(), msgEvent(msg))

	got := consumeOne(t, mb)
	if got.Content != "what is this\n[image: cat.png]" || len(got.Media) != 1 {
		t.Fatalf("inbound %+v", got)
	}
	path, err := store.Resolve(got.Media[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFile(t, path) })
	if data, err := os.ReadFile(path); err != nil || string(data) != "PNGDATA" {
		t.Fatalf("stored %q, %v", data, err)
	}
}

// A media message downloaded but then not handed on leaves no file behind:
// its store entry is released, or the file handed on by path is removed, and
// the event is left for the next channel to fetch again.
func TestHandleMessageEvent_NotHandedOnDiscardsMedia(t *testing.T) {
	tests := []struct {
		name      string
		storeFail bool
		cancelCtx bool
	}{
		{name: "bus closed, stored media", storeFail: false},
		{name: "bus closed, media by path", storeFail: true},
		{name: "stopping during download, stored media", cancelCtx: true},
		{name: "stopping during download, media by path", storeFail: true, cancelCtx: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hs := newStubHomeserver(t)
			hs.download = func(w http.ResponseWriter, _ *http.Request) {
				if tt.cancelCtx {
					cancel()
				}
				reply(t, w, "PNGDATA")
			}
			mb := bus.NewMessageBus()
			c := stubChannel(t, hs, mb, nil)
			inner := media.NewFileMediaStore()
			store := &recordingStore{MediaStore: inner, fail: tt.storeFail}
			c.SetMediaStore(store)
			if !tt.cancelCtx {
				mb.Close()
			} else {
				defer mb.Close()
			}

			evt := msgEvent(imageMsg())
			c.handleMessageEvent(ctx, evt)

			if c.account.handled(evt.ID) {
				t.Fatal("an event not handed on was recorded as handled")
			}
			if tt.cancelCtx {
				if got := drainInbound(t, mb); len(got) != 0 {
					t.Fatalf("delivered %q while stopping", got)
				}
			}
			for _, p := range store.stored() {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("media file %s left behind: %v", p, err)
				}
			}
			if !tt.cancelCtx && len(store.stored()) != 1 {
				t.Fatalf("stored %q, want one download", store.stored())
			}
		})
	}
}

// discardInboundMedia releases a stored ref's scope, removes a file handed on
// by path, and ignores a file already gone.
func TestDiscardInboundMedia(t *testing.T) {
	hs := newStubHomeserver(t)
	c := stubChannel(t, hs, nil, nil)
	dir := t.TempDir()

	stored := dir + "/stored.png"
	byPath := dir + "/bypath.png"
	for _, p := range []string{stored, byPath} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)
	ref, err := store.Store(stored, media.MediaMeta{}, "scope")
	if err != nil {
		t.Fatal(err)
	}

	c.discardInboundMedia([]string{ref, byPath, dir + "/gone.png"}, "scope")

	if _, err := store.Resolve(ref); err == nil {
		t.Error("stored ref still resolves")
	}
	for _, p := range []string{stored, byPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
}
