package discord

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/channels/internal/fdcheck"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
)

// Send posts the text to the chat, as a reply when it answers a message.
func TestSend_PostsTextAndReplies(t *testing.T) {
	f := newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "10", Content: "plain"}); err != nil {
		t.Fatalf("Send plain = %v", err)
	}
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "10", Content: "answer", ReplyToMessageID: "77"}); err != nil {
		t.Fatalf("Send reply = %v", err)
	}
	got := f.recorded()
	want := []fakeCall{
		{method: http.MethodPost, channelID: "10", content: "plain"},
		{method: http.MethodPost, channelID: "10", content: "answer", replyTo: "77"},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The refusals before anything is posted.
func TestSend_Preconditions(t *testing.T) {
	f := newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})

	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "", Content: "x"}); err == nil {
		t.Error("empty chat: nil error")
	}
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "10"}); err != nil {
		t.Errorf("empty content = %v, want nil", err)
	}
	c.SetRunning(false)
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "10", Content: "x"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("stopped = %v, want ErrNotRunning", err)
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("calls = %d, want none", n)
	}
}

// A refused send is classified: an unknown channel is ErrRecipientNotFound.
func TestSend_RefusedIsClassified(t *testing.T) {
	f := newFakeDiscord(t)
	f.writeStatus = http.StatusNotFound
	c, _ := newFakeChannel(t, config.DiscordConfig{})

	err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "10", Content: "x"})
	if !errors.Is(err, channels.ErrRecipientNotFound) {
		t.Fatalf("Send = %v, want ErrRecipientNotFound", err)
	}
}

// Send and SendMedia stop waiting for Discord when their context ends, and
// SendMedia closes the files it opened.
func TestSend_HonoursContext(t *testing.T) {
	f := newFakeDiscord(t)
	f.hangWrites()
	c, _ := newFakeChannel(t, config.DiscordConfig{})
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)
	ref, path := storeDiscordFile(t, store, "a.txt", "alpha")

	// cancelOnArrival cancels the send's context once its request is at
	// Discord, which never answers.
	cancelOnArrival := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer cancel()
			select {
			case <-f.arrived:
			case <-time.After(5 * time.Second):
				t.Error("the request never reached the fake")
			}
		}()
		return ctx
	}

	if err := c.Send(cancelOnArrival(), bus.OutboundMessage{ChatID: "10", Content: "x"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Send = %v, want context.Canceled", err)
	}
	// Another chat: discordgo holds a chat's rate-limit bucket while the
	// first request hangs.
	err := c.SendMedia(cancelOnArrival(), bus.OutboundMediaMessage{ChatID: "11", Parts: []bus.MediaPart{{Type: "file", Ref: ref}}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("SendMedia = %v, want context.Canceled", err)
	}
	if open, ok := fdcheck.OpenFor(path); ok && open {
		t.Error("file left open after an abandoned send")
	}
}

// EditMessage edits the message in place.
func TestEditMessage(t *testing.T) {
	f := newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})

	if err := c.EditMessage(context.Background(), "10", "55", "final"); err != nil {
		t.Fatalf("EditMessage = %v", err)
	}
	got := f.recorded()
	want := fakeCall{method: http.MethodPatch, channelID: "10", messageID: "55", content: "final"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}

	f.mu.Lock()
	f.writeStatus = http.StatusNotFound
	f.mu.Unlock()
	if err := c.EditMessage(context.Background(), "10", "55", "x"); err == nil {
		t.Error("refused edit: nil error")
	}
}

// SendPlaceholder posts the configured text (or the default) when enabled
// and returns its id; disabled, it posts nothing.
func TestSendPlaceholder(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.PlaceholderConfig
		status   int
		wantID   string
		wantText string
		wantErr  bool
	}{
		{name: "disabled", cfg: config.PlaceholderConfig{}},
		{name: "default text", cfg: config.PlaceholderConfig{Enabled: true}, wantID: "posted-1", wantText: "Thinking... 💭"},
		{name: "custom text", cfg: config.PlaceholderConfig{Enabled: true, Text: "Working"}, wantID: "posted-1", wantText: "Working"},
		{name: "refused", cfg: config.PlaceholderConfig{Enabled: true}, status: http.StatusNotFound, wantText: "Thinking... 💭", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDiscord(t)
			f.writeStatus = tt.status
			c, _ := newFakeChannel(t, config.DiscordConfig{Placeholder: tt.cfg})

			id, err := c.SendPlaceholder(context.Background(), "10")
			if (err != nil) != tt.wantErr {
				t.Fatalf("SendPlaceholder err = %v, want error %v", err, tt.wantErr)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			got := f.recorded()
			if tt.wantText == "" {
				if len(got) != 0 {
					t.Errorf("calls = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].content != tt.wantText || got[0].channelID != "10" {
				t.Errorf("calls = %+v, want one post of %q to 10", got, tt.wantText)
			}
		})
	}
}

func waitTyping(t *testing.T, f *fakeDiscord, want string) {
	t.Helper()
	select {
	case got := <-f.typing:
		if got != want {
			t.Fatalf("typing in %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no typing indicator in %q", want)
	}
}

// StartTyping shows the indicator at once; starting again for the same chat
// replaces the loop, and the stop function is idempotent.
func TestStartTyping(t *testing.T) {
	f := newFakeDiscord(t)
	f.typingFail = true // a failing indicator is only logged
	c, _ := newFakeChannel(t, config.DiscordConfig{})

	stop1, err := c.StartTyping(context.Background(), "10")
	if err != nil {
		t.Fatalf("StartTyping = %v", err)
	}
	waitTyping(t, f, "10")
	c.typingMu.Lock()
	first := c.typingStop["10"]
	c.typingMu.Unlock()

	stop2, err := c.StartTyping(context.Background(), "10")
	if err != nil {
		t.Fatalf("StartTyping again = %v", err)
	}
	waitTyping(t, f, "10")
	select {
	case <-first:
	default:
		t.Fatal("starting again did not stop the earlier loop")
	}

	stop2()
	stop2()
	stop1()
	c.typingMu.Lock()
	n := len(c.typingStop)
	c.typingMu.Unlock()
	if n != 0 {
		t.Fatalf("typing loops left = %d, want 0", n)
	}
}

// Stop ends every typing loop, cancels the channel context and closes the
// session.
func TestStop(t *testing.T) {
	f := newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx, c.cancel = ctx, cancel

	if _, err := c.StartTyping(context.Background(), "10"); err != nil {
		t.Fatal(err)
	}
	waitTyping(t, f, "10")
	c.typingMu.Lock()
	loop := c.typingStop["10"]
	c.typingMu.Unlock()

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if c.IsRunning() {
		t.Error("still running after Stop")
	}
	select {
	case <-loop:
	default:
		t.Error("typing loop not stopped")
	}
	if ctx.Err() == nil {
		t.Error("channel context not cancelled")
	}
	if len(c.typingStop) != 0 {
		t.Errorf("typing loops left = %d", len(c.typingStop))
	}

	// A channel that never started has no cancel to call.
	c.cancel = nil
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop again = %v", err)
	}
}
