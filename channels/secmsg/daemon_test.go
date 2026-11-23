package secmsg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	smclient "github.com/tenebris-tech/secmsg/client"
	"github.com/tenebris-tech/secmsg/schema"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/utils"
)

// startChannel builds a channel for account on d via NewFromConfig, starts it,
// and stops it at cleanup.
func startChannel(t *testing.T, d *fakeDaemon, account string, b *bus.MessageBus) *SecMsgChannel {
	t.Helper()
	ch, err := NewFromConfig(
		config.SecMsgConfig{Address: d.addr()},
		config.SecMsgAccountConfig{Account: account, AllowFrom: []string{"*"}},
		b,
	)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	c, ok := ch.(*SecMsgChannel)
	if !ok {
		t.Fatalf("NewFromConfig returned %T", ch)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return c
}

// waitFor polls cond until it holds or fails the test after a generous bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// subscribedAccounts decodes the account filter of a subscribe request.
func subscribedAccounts(t *testing.T, req fakeRequest) []string {
	t.Helper()
	var p struct {
		Accounts []string `json:"accounts"`
	}
	if err := json.Unmarshal(req.params, &p); err != nil {
		t.Fatalf("decode subscribe params: %v", err)
	}
	return p.Accounts
}

func decodeParams(t *testing.T, req fakeRequest) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(req.params, &m); err != nil {
		t.Fatalf("decode %s params: %v", req.method, err)
	}
	return m
}

func TestDiscoverAccounts(t *testing.T) {
	t.Run("returns only linked accounts", func(t *testing.T) {
		d := newFakeDaemon(t, "signal")
		d.linkedAccounts("alice", "bob")
		got, err := DiscoverAccounts(context.Background(), d.addr())
		if err != nil {
			t.Fatalf("DiscoverAccounts: %v", err)
		}
		if want := []string{"alice", "bob"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("accounts = %v, want %v", got, want)
		}
		if p := decodeParams(t, d.expect(schema.MethodStatus)); len(p) != 0 {
			t.Fatalf("status must ask for all accounts, got params %v", p)
		}
	})
	t.Run("address required", func(t *testing.T) {
		if _, err := DiscoverAccounts(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "address is required") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dial failure", func(t *testing.T) {
		if _, err := DiscoverAccounts(context.Background(), closedAddr(t)); err == nil || !strings.HasPrefix(err.Error(), "dial: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("status failure", func(t *testing.T) {
		d := newFakeDaemon(t, "signal")
		d.handle(schema.MethodStatus, func(json.RawMessage) (any, *fakeRPCError) {
			return nil, &fakeRPCError{Code: -32000, Message: "boom"}
		})
		_, err := DiscoverAccounts(context.Background(), d.addr())
		if err == nil || !strings.HasPrefix(err.Error(), "status: ") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNewFromConfig(t *testing.T) {
	if _, err := NewFromConfig(config.SecMsgConfig{}, config.SecMsgAccountConfig{}, bus.NewMessageBus()); err == nil {
		t.Fatal("expected error for missing address")
	}
	ch, err := NewFromConfig(config.SecMsgConfig{Address: "127.0.0.1:1", Name: "sig"}, config.SecMsgAccountConfig{Account: "acct"}, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	c, ok := ch.(*SecMsgChannel)
	if !ok {
		t.Fatalf("NewFromConfig returned %T", ch)
	}
	if c.addr != "127.0.0.1:1" || c.wantAccount != "acct" {
		t.Fatalf("addr=%q wantAccount=%q", c.addr, c.wantAccount)
	}
	if c.MaxMessageLength() != maxMessageLen {
		t.Fatalf("max message length = %d", c.MaxMessageLength())
	}
}

func TestResolveAccount(t *testing.T) {
	tests := []struct {
		name    string
		want    string // configured account
		linked  []string
		statErr bool
		account string
		errHas  string
	}{
		{name: "configured account wins", want: "pinned", linked: []string{"alice", "bob"}, account: "pinned"},
		{name: "single linked account auto-selected", linked: []string{"alice"}, account: "alice"},
		{name: "no linked account", errHas: "signal daemon has no linked account"},
		{name: "several linked accounts", linked: []string{"alice", "bob"}, errHas: `signal daemon has multiple accounts [alice bob]; set "account" in config`},
		{name: "status error", statErr: true, errHas: "status: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t, "signal")
			d.linkedAccounts(tt.linked...)
			if tt.statErr {
				d.handle(schema.MethodStatus, func(json.RawMessage) (any, *fakeRPCError) {
					return nil, &fakeRPCError{Code: -32000, Message: "down"}
				})
			}
			cl, err := smclient.Dial(d.addr())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer utils.CloseQuietly(cl)

			c := &SecMsgChannel{wantAccount: tt.want, ctx: context.Background()}
			got, err := c.resolveAccount(cl, cl.Service())
			if tt.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("err = %v, want containing %q", err, tt.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAccount: %v", err)
			}
			if got != tt.account {
				t.Fatalf("account = %q, want %q", got, tt.account)
			}
			if tt.want != "" {
				// An explicit account must not cost a status round-trip.
				select {
				case req := <-d.reqs:
					t.Fatalf("unexpected RPC %q for a configured account", req.method)
				default:
				}
			}
		})
	}
}

// A started channel subscribes for its account, delivers inbound messages to
// the bus, and routes replies to the 1:1 or group send method.
func TestChannel_ConnectReceiveAndSend(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	b := bus.NewMessageBus()
	c := startChannel(t, d, "acct", b)

	sub := d.expect(schema.MethodSubscribe)
	if got := subscribedAccounts(t, sub); !reflect.DeepEqual(got, []string{"acct"}) {
		t.Fatalf("subscribed accounts = %v", got)
	}

	// Undecodable params and non-message notifications are skipped; the valid
	// message after them is still delivered.
	sub.conn.notify(t, schema.MethodMessage, "not an object")
	sub.conn.notify(t, schema.MethodTyping, map[string]any{"action": "started"})
	m := directMsg()
	m.Body = "hi there"
	sub.conn.notify(t, schema.MethodMessage, m)

	in, ok := consume(t, b)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if in.Content != "hi there" || in.ChatID != "uuid-alice" || in.Sender.CanonicalID != "signal:uuid-alice" {
		t.Fatalf("inbound = %+v", in)
	}
	if in.Metadata["service"] != "signal" || in.Metadata["account"] != "acct" || in.Metadata["is_group"] != "false" {
		t.Fatalf("metadata = %v", in.Metadata)
	}
	if _, ok := consume(t, b); ok {
		t.Fatal("skipped notifications must not reach the bus")
	}

	ctx := context.Background()

	// Empty content is a no-op: nothing reaches the daemon.
	if err := c.Send(ctx, bus.OutboundMessage{ChatID: "uuid-alice"}); err != nil {
		t.Fatalf("empty send: %v", err)
	}

	if err := c.Send(ctx, bus.OutboundMessage{ChatID: "uuid-alice", Content: "direct reply"}); err != nil {
		t.Fatalf("direct send: %v", err)
	}
	want := map[string]any{"service": "signal", "account": "acct", "to": "uuid-alice", "body": "direct reply"}
	if got := decodeParams(t, d.expect(schema.MethodSend)); !reflect.DeepEqual(got, want) {
		t.Fatalf("send params = %v, want %v", got, want)
	}

	if err := c.Send(ctx, bus.OutboundMessage{ChatID: groupChatPrefix + "GROUPHEX", Content: "group reply"}); err != nil {
		t.Fatalf("group send: %v", err)
	}
	want = map[string]any{"service": "signal", "account": "acct", "group_id": "GROUPHEX", "body": "group reply"}
	if got := decodeParams(t, d.expect(schema.MethodSendGroup)); !reflect.DeepEqual(got, want) {
		t.Fatalf("send.group params = %v, want %v", got, want)
	}

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Send(ctx, bus.OutboundMessage{ChatID: "uuid-alice", Content: "late"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("send after stop: err = %v, want ErrNotRunning", err)
	}
}

func TestChannel_SendErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		chatID string
		code   int
		want   error
	}{
		{"direct stealth is receive-only", schema.MethodSend, "uuid-alice", schema.ErrCodeStealth, channels.ErrReceiveOnly},
		{"direct failure", schema.MethodSend, "uuid-alice", -32000, channels.ErrSendFailed},
		{"group stealth is receive-only", schema.MethodSendGroup, groupChatPrefix + "G", schema.ErrCodeStealth, channels.ErrReceiveOnly},
		{"group failure", schema.MethodSendGroup, groupChatPrefix + "G", -32000, channels.ErrSendFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t, "signal")
			d.handle(tt.method, func(json.RawMessage) (any, *fakeRPCError) {
				return nil, &fakeRPCError{Code: tt.code, Message: "refused"}
			})
			c := startChannel(t, d, "acct", bus.NewMessageBus())
			d.expect(schema.MethodSubscribe)

			err := c.Send(context.Background(), bus.OutboundMessage{ChatID: tt.chatID, Content: "x"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Fatalf("daemon detail lost: %v", err)
			}
		})
	}
}

// While the channel is running but not yet connected, Send reports
// ErrNotRunning so the caller can retry.
func TestSend_NotConnected(t *testing.T) {
	c := newTestChannel(bus.NewMessageBus(), []string{"*"})
	c.SetRunning(true)
	if err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "x", Content: "y"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

// With no configured account the channel binds the daemon's sole linked
// account, and link RPCs then use it.
func TestChannel_AutoSelectsLinkedAccount(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	d.linkedAccounts("alice")
	c := startChannel(t, d, "", bus.NewMessageBus())

	d.expect(schema.MethodStatus)
	if got := subscribedAccounts(t, d.expect(schema.MethodSubscribe)); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Fatalf("subscribed accounts = %v", got)
	}
	if got := c.linkAccount(); got != "alice" {
		t.Fatalf("linkAccount = %q, want alice", got)
	}
}

// A daemon that cannot bind an account is reported as a connection failure.
func TestChannel_UnresolvableAccountReportsOutage(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	d.linkedAccounts() // none linked
	c := startChannel(t, d, "", bus.NewMessageBus())
	d.expect(schema.MethodStatus)
	waitFor(t, "outage reported", func() bool { return !c.ConnDownSince().IsZero() })
}

// A refused subscription is reported as a connection failure.
func TestChannel_SubscribeErrorReportsOutage(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	d.handle(schema.MethodSubscribe, func(json.RawMessage) (any, *fakeRPCError) {
		return nil, &fakeRPCError{Code: -32000, Message: "no"}
	})
	c := startChannel(t, d, "acct", bus.NewMessageBus())
	d.expect(schema.MethodSubscribe)
	waitFor(t, "outage reported", func() bool { return !c.ConnDownSince().IsZero() })
	c.mu.RLock()
	cl := c.client
	c.mu.RUnlock()
	if cl != nil {
		t.Fatal("client must be cleared after a failed session")
	}
}

// A dropped daemon connection is reported and the channel reconnects and
// resubscribes, ending the outage.
func TestChannel_ReconnectsAfterDroppedConnection(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	c := startChannel(t, d, "acct", bus.NewMessageBus())

	// Close only after the subscribe reply is on the wire, so the session is
	// established and then dropped (not refused).
	sub := d.expect(schema.MethodSubscribe)
	d.waitAnswered(sub)

	if err := sub.conn.conn.Close(); err != nil {
		t.Fatalf("close daemon side: %v", err)
	}
	waitFor(t, "outage reported", func() bool { return !c.ConnDownSince().IsZero() })

	d.expect(schema.MethodSubscribe)
	waitFor(t, "outage ended", func() bool { return c.ConnDownSince().IsZero() })
}

func TestLinking(t *testing.T) {
	d := newFakeDaemon(t, "signal")
	d.handle(schema.MethodLinkRequest, func(json.RawMessage) (any, *fakeRPCError) {
		return schema.LinkReply{Status: schema.LinkStatusPending, URI: "sgnl://linkdevice?x"}, nil
	})
	d.handle(schema.MethodLinkStatus, func(json.RawMessage) (any, *fakeRPCError) {
		return schema.LinkReply{Status: schema.LinkStatusComplete, Phone: "+15550001111"}, nil
	})
	c := &SecMsgChannel{addr: d.addr(), wantAccount: "acct"}
	ctx := context.Background()

	reply, err := c.RequestLink(ctx)
	if err != nil {
		t.Fatalf("RequestLink: %v", err)
	}
	if reply.Status != schema.LinkStatusPending || reply.URI != "sgnl://linkdevice?x" {
		t.Fatalf("link reply = %+v", reply)
	}
	want := map[string]any{"account": "acct", "name": app.Name()}
	if got := decodeParams(t, d.expect(schema.MethodLinkRequest)); !reflect.DeepEqual(got, want) {
		t.Fatalf("link.request params = %v, want %v", got, want)
	}

	state, err := c.LinkState(ctx)
	if err != nil {
		t.Fatalf("LinkState: %v", err)
	}
	if state.Status != schema.LinkStatusComplete || state.Phone != "+15550001111" {
		t.Fatalf("link state = %+v", state)
	}
	if got := decodeParams(t, d.expect(schema.MethodLinkStatus)); !reflect.DeepEqual(got, map[string]any{"account": "acct"}) {
		t.Fatalf("link.status params = %v", got)
	}
}

func TestLinking_DialFailure(t *testing.T) {
	c := &SecMsgChannel{addr: closedAddr(t), wantAccount: "acct"}
	if _, err := c.RequestLink(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "dial: ") {
		t.Fatalf("RequestLink err = %v", err)
	}
	if _, err := c.LinkState(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "dial: ") {
		t.Fatalf("LinkState err = %v", err)
	}
}

func TestHandleMessage_GroupTriggerPrefix(t *testing.T) {
	b := bus.NewMessageBus()
	c := newTestChannel(b, []string{"*"})
	channels.WithGroupTrigger(config.GroupTriggerConfig{Prefixes: []string{"!bot"}})(c.BaseChannel)

	m := directMsg()
	m.To = schema.Party{ID: "GROUPHEX"}
	m.Body = "chatter not for the bot"
	c.handleMessage(m)
	if _, ok := consume(t, b); ok {
		t.Fatal("group message without the trigger prefix must be dropped")
	}

	m.Body = "!bot what time is it"
	c.handleMessage(m)
	in, ok := consume(t, b)
	if !ok {
		t.Fatal("expected triggered group message")
	}
	if in.Content != "what time is it" || in.Metadata["is_group"] != "true" {
		t.Fatalf("content=%q metadata=%v", in.Content, in.Metadata)
	}
}

func TestHandleMessage_AttachmentsWithStore(t *testing.T) {
	b := bus.NewMessageBus()
	c := newTestChannel(b, []string{"*"})
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)

	existing := filepath.Join(t.TempDir(), "pic.jpg")
	if err := os.WriteFile(existing, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone.bin")

	m := directMsg()
	m.Body = "look"
	m.Attachments = []schema.Attachment{
		{LocalPath: existing, FileName: "pic.jpg", ContentType: "image/jpeg"},
		{FileName: "not-downloaded.txt"}, // no local file: skipped
		{LocalPath: missing, ContentType: "application/octet-stream"},
	}
	c.handleMessage(m)

	in, ok := consume(t, b)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if len(in.Media) != 2 {
		t.Fatalf("media = %v", in.Media)
	}
	// A stored file is carried as a store ref that resolves to the daemon path.
	if !strings.HasPrefix(in.Media[0], "media://") {
		t.Fatalf("stored attachment ref = %q", in.Media[0])
	}
	path, meta, err := store.ResolveWithMeta(in.Media[0])
	if err != nil || path != existing || meta.Filename != "pic.jpg" || meta.ContentType != "image/jpeg" || meta.Source != "secmsg" {
		t.Fatalf("resolve = %q %+v %v", path, meta, err)
	}
	// A file the store refuses falls back to the raw daemon path.
	if in.Media[1] != missing {
		t.Fatalf("unstored attachment = %q, want %q", in.Media[1], missing)
	}
	if want := "look\n[attachment: pic.jpg]\n[attachment: application/octet-stream]"; in.Content != want {
		t.Fatalf("content = %q, want %q", in.Content, want)
	}
}

// Before connecting, a channel without a configured account has no link
// account; the configured one is used when set.
func TestLinkAccount(t *testing.T) {
	if got := (&SecMsgChannel{wantAccount: "acct", account: "other"}).linkAccount(); got != "acct" {
		t.Fatalf("configured: %q", got)
	}
	if got := (&SecMsgChannel{}).linkAccount(); got != "" {
		t.Fatalf("unresolved: %q", got)
	}
}

// Without a resolved service the channel name stands in as the platform, so
// canonical IDs stay well-formed.
func TestHandleMessage_PlatformFallsBackToChannelName(t *testing.T) {
	b := bus.NewMessageBus()
	c := newTestChannel(b, []string{"*"})
	c.service = ""
	c.handleMessage(directMsg())
	in, ok := consume(t, b)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if in.Sender.Platform != c.Name() || in.Sender.CanonicalID != c.Name()+":uuid-alice" {
		t.Fatalf("sender = %+v", in.Sender)
	}
}

func TestHandleEnvelope_Nil(t *testing.T) {
	b := bus.NewMessageBus()
	c := newTestChannel(b, []string{"*"})
	c.handleEnvelope(nil)
	if _, ok := consume(t, b); ok {
		t.Fatal("nil envelope must be ignored")
	}
}
