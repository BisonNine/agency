package bus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPublishOutboundMediaSubscribe(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()
	ctx := context.Background()

	msg := OutboundMediaMessage{Channel: "telegram", ChatID: "c1", Parts: []MediaPart{{Type: "image", Ref: "media://a"}}}
	if err := mb.PublishOutboundMedia(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, ok := mb.SubscribeOutboundMedia(ctx)
	if !ok || got.ChatID != "c1" || len(got.Parts) != 1 || got.Parts[0].Ref != "media://a" {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

func TestOutboundPublish_Refusals(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	open := NewMessageBus()
	defer open.Close()
	if err := open.PublishOutbound(cancelled, OutboundMessage{}); !errors.Is(err, context.Canceled) {
		t.Errorf("outbound with cancelled ctx = %v", err)
	}
	if err := open.PublishOutboundMedia(cancelled, OutboundMediaMessage{}); !errors.Is(err, context.Canceled) {
		t.Errorf("media with cancelled ctx = %v", err)
	}

	closed := NewMessageBus()
	closed.Close()
	if err := closed.PublishOutboundMedia(context.Background(), OutboundMediaMessage{}); !errors.Is(err, ErrBusClosed) {
		t.Errorf("media on closed bus = %v", err)
	}
}

// A publisher blocked on a full buffer gives up when its context ends.
func TestOutboundPublish_FullBufferTimesOut(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()
	ctx := context.Background()
	for range defaultBusBufferSize {
		if err := mb.PublishOutbound(ctx, OutboundMessage{}); err != nil {
			t.Fatal(err)
		}
		if err := mb.PublishOutboundMedia(ctx, OutboundMediaMessage{}); err != nil {
			t.Fatal(err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := mb.PublishOutbound(short, OutboundMessage{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("outbound full = %v", err)
	}
	short2, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	if err := mb.PublishOutboundMedia(short2, OutboundMediaMessage{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("media full = %v", err)
	}
}

// A publisher blocked on a full buffer is released with ErrBusClosed, or its
// message is drained and reported undelivered, when the bus closes.
func TestOutboundPublish_BlockedThenClosed(t *testing.T) {
	mb := NewMessageBus()
	ctx := context.Background()
	for range defaultBusBufferSize {
		if err := mb.PublishOutbound(ctx, OutboundMessage{}); err != nil {
			t.Fatal(err)
		}
		if err := mb.PublishOutboundMedia(ctx, OutboundMediaMessage{}); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 2)
	go func() { errs <- mb.PublishOutbound(ctx, OutboundMessage{}) }()
	go func() { errs <- mb.PublishOutboundMedia(ctx, OutboundMediaMessage{}) }()
	mb.Close()
	for range 2 {
		if err := <-errs; err != nil && !errors.Is(err, ErrBusClosed) {
			t.Errorf("blocked publish = %v, want nil or ErrBusClosed", err)
		}
	}
}

func TestOutboundSubscribe_EndsOnCancelAndClose(t *testing.T) {
	mb := NewMessageBus()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := mb.SubscribeOutbound(cancelled); ok {
		t.Error("SubscribeOutbound with cancelled ctx returned ok")
	}
	if _, ok := mb.SubscribeOutboundMedia(cancelled); ok {
		t.Error("SubscribeOutboundMedia with cancelled ctx returned ok")
	}
	mb.Close()
	if _, ok := mb.SubscribeOutboundMedia(context.Background()); ok {
		t.Error("SubscribeOutboundMedia on closed bus returned ok")
	}
}

func TestSetSpawnDepth(t *testing.T) {
	if got := SetSpawnDepth(nil, 0); got != nil {
		t.Errorf("depth 0 on nil = %v, want nil", got)
	}
	m := map[string]string{"a": "b"}
	if got := SetSpawnDepth(m, -1); len(got) != 1 {
		t.Errorf("negative depth added a key: %v", got)
	}
	got := SetSpawnDepth(nil, 3)
	if got[MetaSpawnDepth] != "3" {
		t.Errorf("depth 3 on nil = %v", got)
	}
	got = SetSpawnDepth(m, 2)
	if got[MetaSpawnDepth] != "2" || got["a"] != "b" {
		t.Errorf("depth 2 on existing = %v", got)
	}
}

func TestInboundMessageReplyRequired(t *testing.T) {
	if (InboundMessage{}).ReplyRequired() {
		t.Error("no metadata: ReplyRequired = true")
	}
	if !(InboundMessage{Metadata: map[string]string{MetaReplyRequired: "1"}}).ReplyRequired() {
		t.Error("reply_required=1: ReplyRequired = false")
	}
	if (InboundMessage{Metadata: map[string]string{MetaReplyRequired: "true"}}).ReplyRequired() {
		t.Error("reply_required=true: ReplyRequired = true, want only \"1\"")
	}
}
