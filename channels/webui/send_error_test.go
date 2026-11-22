package webui

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
)

// A reply to a WebUI session no browser has open is ErrRecipientOffline.
func TestSend_NoOpenSessionIsOffline(t *testing.T) {
	c := testChannel(t, "tok")
	c.SetRunning(true)
	err := c.Send(context.Background(), bus.OutboundMessage{Channel: "webui", ChatID: "webui:nosuch", Content: "hello"})
	if !errors.Is(err, channels.ErrRecipientOffline) {
		t.Fatalf("Send = %v, want ErrRecipientOffline", err)
	}
}
