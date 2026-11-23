// ClawEh
// License: MIT

package device

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/gatewayproto"
	"go.bison9.com/agency/utils"
)

// listenerlessDevice is the device channel without its listener: Start and
// Stop do nothing, so the manager can run it in a test.
type listenerlessDevice struct{ *DeviceChannel }

func (listenerlessDevice) Start(context.Context) error { return nil }
func (listenerlessDevice) Stop(context.Context) error  { return nil }

// A reply the device channel cannot deliver reaches the sender's OnDelivery,
// through the manager, with why: no such device, or a paired device that is
// not connected or whose connection is closing.
func TestDeliverReply_ReasonsThroughManager(t *testing.T) {
	srv, store, _ := newTestServer(t, ServerOptions{ServerVersion: "test-1"})
	ctx := context.Background()

	// A paired device that is not connected.
	reqID, err := store.CreatePending(ctx, PendingPairing{DeviceID: "paired-1", PublicKey: "pk", Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Approve(ctx, reqID, []string{"operator"}, nil); err != nil {
		t.Fatal(err)
	}
	// A connection that is closing: its writer has already closed.
	cw := &connWriter{queue: make(chan any, 1), done: make(chan struct{})}
	close(cw.done)
	srv.conns.Store("device:broken", &liveConn{cw: cw, deviceID: "broken", chatID: "device:broken"})

	dc := &DeviceChannel{BaseChannel: channels.NewBaseChannel("device", nil, nil, nil), server: srv, store: store}
	dc.SetRunning(true)
	mb := bus.NewMessageBus()
	t.Cleanup(mb.Close)
	m, err := channels.NewManager(&config.Config{}, mb, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.RegisterChannel("device", listenerlessDevice{dc})
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.StopAll(context.Background()); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})

	tests := []struct {
		name   string
		chatID string
		want   error
	}{
		{"unknown device", "device:nosuch", channels.ErrRecipientNotFound},
		{"not a device chat", "telegram:1", channels.ErrRecipientNotFound},
		{"not connected", "device:paired-1", channels.ErrRecipientOffline},
		{"connection closing", "device:broken", channels.ErrRecipientOffline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan error, 1)
			err := mb.PublishOutbound(ctx, bus.OutboundMessage{
				Channel: "device", ChatID: tt.chatID, Content: "hello",
				OnDelivery: func(err error) { got <- err },
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-got:
				if !errors.Is(err, tt.want) {
					t.Fatalf("delivery error = %v, want %v", err, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("OnDelivery was not called")
			}
		})
	}
}

// A fault of the server itself (the pairing store fails) is ErrSendFailed.
func TestDeliverReply_StoreFaultIsSendFailed(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, ServerOptions{ServerVersion: "test-1"})
	if err := srv.DeliverReply(context.Background(), "device:paired-1", "", "hello", nil, ""); !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("DeliverReply = %v, want ErrSendFailed", err)
	}
}

// A send cut short by shutdown returns the context's error, never a
// delivery reason the manager would alert on.
func TestDeviceSend_ShutdownReturnsContextError(t *testing.T) {
	srv, store, _ := newTestServer(t, ServerOptions{ServerVersion: "test-1"})
	dc := &DeviceChannel{BaseChannel: channels.NewBaseChannel("device", nil, nil, nil), server: srv, store: store}
	dc.SetRunning(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := dc.Send(ctx, bus.OutboundMessage{Channel: "device", ChatID: "device:nosuch", Content: "hello"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("Send = %v, want context.Canceled", err)
	}
}

// helloHoldListener hands out connections whose write of the hello-ok
// response returns only once release is closed. The bytes reach the client
// first, so the test can act after the device has seen hello-ok and before
// the server goes on from that write.
type helloHoldListener struct {
	net.Listener
	written chan struct{} // closed once hello-ok is on the wire
	release chan struct{}
	once    sync.Once
}

func (l *helloHoldListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &helloHoldConn{Conn: c, l: l}, nil
}

type helloHoldConn struct {
	net.Conn
	l *helloHoldListener
}

func (c *helloHoldConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if bytes.Contains(b, []byte(`"hello-ok"`)) {
		c.l.once.Do(func() { close(c.l.written) })
		<-c.l.release
	}
	return n, err
}

// A device is deliverable from the moment it has been sent hello-ok: a reply
// sent to it right then is delivered, not refused as offline.
func TestDeliverReply_DeliverableOnceHelloOkSent(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	srv := NewServer(store, ServerOptions{ServerVersion: "test-1", AutoApprove: true})
	hs := httptest.NewUnstartedServer(http.HandlerFunc(srv.HandleWS))
	hold := &helloHoldListener{Listener: hs.Listener, written: make(chan struct{}), release: make(chan struct{})}
	hs.Listener = hold
	hs.Start()
	t.Cleanup(hs.Close)
	released := sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(released)

	alice := newEmulator(t)
	opened := make(chan *websocket.Conn, 1)
	go func() {
		conn, r := alice.open(t, "ws"+strings.TrimPrefix(hs.URL, "http"), "")
		if !r.OK {
			t.Errorf("connect refused: %+v", r.Error)
		}
		opened <- conn
	}()
	select {
	case <-hold.written:
	case <-time.After(5 * time.Second):
		t.Fatal("hello-ok was never written")
	}
	conn := <-opened // alice has read hello-ok; the server is still in that write
	t.Cleanup(func() { utils.CloseQuietly(conn) })

	if err := srv.DeliverReply(context.Background(), "device:"+alice.deviceID(), "", "hello", nil, ""); err != nil {
		t.Fatalf("DeliverReply right after hello-ok: %v", err)
	}
	released()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var f resFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("device did not get the reply: %v", err)
		}
		if f.Type == gatewayproto.FrameEvent && f.Event == "chat" {
			return
		}
	}
}
