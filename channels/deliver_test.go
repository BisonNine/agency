package channels

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/media"
)

// fakeRecorder is a PlaceholderRecorder that remembers what a channel recorded.
type fakeRecorder struct {
	mu           sync.Mutex
	placeholders []string // "channel/chat/id"
	typing       []string // "channel/chat"
	reactions    []string // "channel/chat/message"
}

func (r *fakeRecorder) RecordPlaceholder(channel, chatID, placeholderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.placeholders = append(r.placeholders, channel+"/"+chatID+"/"+placeholderID)
}

func (r *fakeRecorder) RecordTypingStop(channel, chatID string, _ func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.typing = append(r.typing, channel+"/"+chatID)
}

func (r *fakeRecorder) RecordReactionUndo(channel, chatID, messageID string, _ func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, channel+"/"+chatID+"/"+messageID)
}

// busyChannel shows typing, a reaction and a placeholder for every inbound
// message, each failing when its fail flag is set.
type busyChannel struct {
	*BaseChannel
	failTyping, failReaction, failPlaceholder bool
	placeholderID                             string
}

func (c *busyChannel) Start(context.Context) error                     { return nil }
func (c *busyChannel) Stop(context.Context) error                      { return nil }
func (c *busyChannel) Send(context.Context, bus.OutboundMessage) error { return nil }

func (c *busyChannel) StartTyping(context.Context, string) (func(), error) {
	if c.failTyping {
		return nil, errors.New("typing refused")
	}
	return func() {}, nil
}

func (c *busyChannel) ReactToMessage(context.Context, string, string) (func(), error) {
	if c.failReaction {
		return nil, errors.New("reaction refused")
	}
	return func() {}, nil
}

func (c *busyChannel) SendPlaceholder(context.Context, string) (string, error) {
	if c.failPlaceholder {
		return "", errors.New("placeholder refused")
	}
	return c.placeholderID, nil
}

func newBusyChannel(mb *bus.MessageBus) (*busyChannel, *fakeRecorder) {
	c := &busyChannel{BaseChannel: NewBaseChannel("busy", nil, mb, []string{"*"}), placeholderID: "ph1"}
	rec := &fakeRecorder{}
	c.SetOwner(c)
	c.SetPlaceholderRecorder(rec)
	return c, rec
}

func TestDeliverMessage_RecordsTypingReactionAndPlaceholder(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c, rec := newBusyChannel(mb)

	if err := c.DeliverMessage(context.Background(), bus.Peer{Kind: "direct", ID: "u1"}, "m1", "u1", "chat", "hello", nil, nil); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if _, ok := consumeInbound(t, mb); !ok {
		t.Fatal("message not published")
	}
	if len(rec.typing) != 1 || rec.typing[0] != "busy/chat" {
		t.Errorf("typing = %v, want [busy/chat]", rec.typing)
	}
	if len(rec.reactions) != 1 || rec.reactions[0] != "busy/chat/m1" {
		t.Errorf("reactions = %v, want [busy/chat/m1]", rec.reactions)
	}
	if len(rec.placeholders) != 1 || rec.placeholders[0] != "busy/chat/ph1" {
		t.Errorf("placeholders = %v, want [busy/chat/ph1]", rec.placeholders)
	}
	if c.GetPlaceholderRecorder() != rec {
		t.Error("GetPlaceholderRecorder does not return the injected recorder")
	}
}

func TestDeliverMessage_FailedIndicatorsRecordNothing(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c, rec := newBusyChannel(mb)
	c.failTyping, c.failReaction, c.failPlaceholder = true, true, true

	if err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "u1", "chat", "hello", nil, nil); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if _, ok := consumeInbound(t, mb); !ok {
		t.Fatal("message not published although its indicators failed")
	}
	if len(rec.typing)+len(rec.reactions)+len(rec.placeholders) != 0 {
		t.Fatalf("recorded %v %v %v, want nothing", rec.typing, rec.reactions, rec.placeholders)
	}
}

// A message without an id gets no reaction, and one with audio no
// placeholder (the agent sends it after transcription).
func TestDeliverMessage_NoReactionWithoutIDNoPlaceholderForAudio(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c, rec := newBusyChannel(mb)

	if err := c.DeliverMessage(context.Background(), bus.Peer{}, "", "u1", "chat", "[voice]", nil, nil); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if _, ok := consumeInbound(t, mb); !ok {
		t.Fatal("message not published")
	}
	if len(rec.typing) != 1 || len(rec.reactions) != 0 || len(rec.placeholders) != 0 {
		t.Fatalf("typing %v reactions %v placeholders %v, want typing only", rec.typing, rec.reactions, rec.placeholders)
	}
}

func TestDeliverMessage_EmptyPlaceholderIDNotRecorded(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c, rec := newBusyChannel(mb)
	c.placeholderID = ""

	if err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "u1", "chat", "hi", nil, nil); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if len(rec.placeholders) != 0 {
		t.Fatalf("placeholders = %v, want none", rec.placeholders)
	}
}

func TestDeliverMessage_SenderInfo(t *testing.T) {
	tests := []struct {
		name      string
		allow     []string
		sender    bus.SenderInfo
		published bool
		senderID  string
	}{
		{
			name:      "canonical id allowed and used",
			allow:     []string{"telegram:123"},
			sender:    bus.SenderInfo{Platform: "telegram", PlatformID: "123", CanonicalID: "telegram:123"},
			published: true,
			senderID:  "telegram:123",
		},
		{
			name:      "platform id only keeps the raw sender id",
			allow:     []string{"123"},
			sender:    bus.SenderInfo{Platform: "telegram", PlatformID: "123"},
			published: true,
			senderID:  "raw",
		},
		{
			name:   "sender not allowed",
			allow:  []string{"telegram:999"},
			sender: bus.SenderInfo{Platform: "telegram", PlatformID: "123", CanonicalID: "telegram:123"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mb := bus.NewMessageBus()
			defer mb.Close()
			c := NewBaseChannel("telegram", nil, mb, tt.allow)

			if err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "raw", "chat", "hi", nil, nil, tt.sender); err != nil {
				t.Fatalf("DeliverMessage: %v", err)
			}
			msg, ok := consumeInbound(t, mb)
			if ok != tt.published {
				t.Fatalf("published = %v, want %v", ok, tt.published)
			}
			if ok && (msg.SenderID != tt.senderID || msg.Sender != tt.sender) {
				t.Fatalf("SenderID %q Sender %+v, want %q %+v", msg.SenderID, msg.Sender, tt.senderID, tt.sender)
			}
		})
	}
}

func TestDeliverMessage_SenderIDNotAllowed(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := NewBaseChannel("test", nil, mb, []string{"alice"})

	if err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "bob", "chat", "hi", nil, nil); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if _, ok := consumeInbound(t, mb); ok {
		t.Fatal("message from a sender not allowed was published")
	}
}

func TestDeliverMessage_SessionKeyFromMetadata(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	c := NewBaseChannel("test", nil, mb, []string{"*"})

	meta := map[string]string{"session_key": "agent:alice:main"}
	if err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "u1", "chat", "hi", nil, meta); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	msg, ok := consumeInbound(t, mb)
	if !ok || msg.SessionKey != "agent:alice:main" {
		t.Fatalf("SessionKey = %q (published %v), want agent:alice:main", msg.SessionKey, ok)
	}
}

func TestDeliverMessage_ClosedBus(t *testing.T) {
	mb := bus.NewMessageBus()
	mb.Close()
	c := NewBaseChannel("test", nil, mb, []string{"*"})

	err := c.DeliverMessage(context.Background(), bus.Peer{}, "m1", "u1", "chat", "hi", nil, nil)
	if !errors.Is(err, bus.ErrBusClosed) {
		t.Fatalf("DeliverMessage = %v, want ErrBusClosed", err)
	}
	// HandleMessage logs the same failure and returns.
	c.HandleMessage(context.Background(), bus.Peer{}, "m2", "u1", "chat", "hi", nil, nil)
}

type nopStore struct{ media.MediaStore }

func TestBaseChannel_Accessors(t *testing.T) {
	c := NewBaseChannel("test", nil, nil, nil, WithReasoningChannelID("thoughts"))
	if got := c.ReasoningChannelID(); got != "thoughts" {
		t.Errorf("ReasoningChannelID = %q, want thoughts", got)
	}
	if c.GetMediaStore() != nil || c.GetPlaceholderRecorder() != nil {
		t.Error("a new channel has a media store or placeholder recorder")
	}
	s := nopStore{}
	c.SetMediaStore(s)
	if c.GetMediaStore() != s {
		t.Error("GetMediaStore does not return the injected store")
	}
}
