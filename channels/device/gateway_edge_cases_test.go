package device

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/utils"
)

// newTestChannel builds a device channel over a fresh data dir, stopped when
// the test ends. It is not started.
func newTestChannel(t *testing.T, mb *bus.MessageBus) *DeviceChannel {
	t.Helper()
	dc, err := NewDeviceChannel(config.DeviceChannelConfig{Enabled: true}, t.TempDir(), false, mb, "", nil)
	if err != nil {
		t.Fatalf("NewDeviceChannel: %v", err)
	}
	t.Cleanup(func() {
		if err := dc.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return dc
}

// NewDeviceChannel fails when its state directory or pairing store cannot be
// created.
func TestNewDeviceChannel_Failures(t *testing.T) {
	t.Run("state dir", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, global.InternalDir), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewDeviceChannel(config.DeviceChannelConfig{Enabled: true}, dataDir, false, bus.NewMessageBus(), "", nil)
		if err == nil || !strings.Contains(err.Error(), "create state dir") {
			t.Fatalf("NewDeviceChannel = %v, want a state dir error", err)
		}
	})
	t.Run("store", func(t *testing.T) {
		dataDir := t.TempDir()
		stateDir := filepath.Join(dataDir, global.InternalDir)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "gateway.db"), []byte(strings.Repeat("not a database ", 40)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewDeviceChannel(config.DeviceChannelConfig{Enabled: true}, dataDir, false, bus.NewMessageBus(), "", nil); err == nil {
			t.Fatal("NewDeviceChannel over a corrupt pairing store: want an error")
		}
	})
}

// A turn pinned to an agent's session reaches the bus with that session key
// and the agent preresolved.
func TestInbound_PinsSessionAndAgent(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()
	dc := newTestChannel(t, mb)

	dc.server.inbound("dev1", "device:dev1", "hello", "run1", "agent:bob:main", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, ok := mb.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("no inbound message on the bus")
	}
	if msg.Metadata["session_key"] != "agent:bob:main" || msg.Metadata["preresolved_agent_id"] != "bob" {
		t.Fatalf("metadata = %v, want the session key and agent bob", msg.Metadata)
	}
}

// senderName falls back to the default user when the device record cannot be
// read, and is empty for an unknown device with no default user.
func TestSenderName_Fallbacks(t *testing.T) {
	ctx := context.Background()
	dc := newTestChannel(t, bus.NewMessageBus())

	if got := dc.DefaultUser(); got != "" {
		t.Fatalf("DefaultUser with none set = %q, want empty", got)
	}
	if got := dc.senderName(ctx, "unknown"); got != "" {
		t.Fatalf("senderName(unknown, no default) = %q, want empty", got)
	}

	dc.SetDefaultUser(func() string { return "Alice" })
	utils.CloseQuietly(dc.store)
	if got := dc.senderName(ctx, "dev1"); got != "Alice" {
		t.Fatalf("senderName with an unreadable store = %q, want the default user", got)
	}
}

// failingMediaStore refuses every file.
type failingMediaStore struct{ media.MediaStore }

func (failingMediaStore) Store(string, media.MediaMeta, string) (string, error) {
	return "", errors.New("media store full")
}

// An attachment the media store refuses, or that cannot be staged, is dropped.
func TestStoreInboundAttachments_Failures(t *testing.T) {
	atts := []InboundAttachment{{MimeType: "image/png", Data: []byte{1, 2, 3}}}

	t.Run("media store refuses", func(t *testing.T) {
		staging := t.TempDir()
		utils.SetMediaStagingDir(staging, 0, 0)
		t.Cleanup(func() { utils.SetMediaStagingDir("", 0, 0) })
		dc := newTestChannel(t, bus.NewMessageBus())
		dc.SetMediaStore(failingMediaStore{})
		if refs := dc.storeInboundAttachments("device:dev1", "run1", atts); len(refs) != 0 {
			t.Fatalf("refs = %v, want none", refs)
		}
	})

	t.Run("staging dir unusable", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		utils.SetMediaStagingDir(filepath.Join(parent, "media"), 0, 0)
		t.Cleanup(func() { utils.SetMediaStagingDir("", 0, 0) })
		dc := newTestChannel(t, bus.NewMessageBus())
		dc.SetMediaStore(media.NewFileMediaStore())
		if refs := dc.storeInboundAttachments("device:dev1", "run1", atts); refs != nil {
			t.Fatalf("refs = %v, want none", refs)
		}
	})
}

func TestExtForMIME_Images(t *testing.T) {
	for mime, want := range map[string]string{"image/gif": ".gif", "image/webp": ".webp", "image/heic": ".heic"} {
		if got := extForMIME(mime); got != want {
			t.Errorf("extForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

// The channel hands the querier to its protocol server and reports it back.
func TestDeviceChannel_AgentQuerier(t *testing.T) {
	dc := newTestChannel(t, bus.NewMessageBus())
	if dc.AgentQuerier() != nil {
		t.Fatal("AgentQuerier before SetAgentQuerier: want nil")
	}
	q := stubQuerier{defaultAgent: "alice"}
	dc.SetAgentQuerier(q)
	if got, ok := dc.AgentQuerier().(stubQuerier); !ok || got != q {
		t.Fatalf("AgentQuerier = %#v, want the installed querier", dc.AgentQuerier())
	}
}

// SetTLSConfig(nil) clears an earlier configuration, so a TLS channel refuses
// to start without a certificate.
func TestSetTLSConfig_NilClears(t *testing.T) {
	dc := newTestChannel(t, bus.NewMessageBus())
	dc.useTLS = true
	dc.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	dc.SetTLSConfig(nil)
	if err := dc.Start(context.Background()); !errors.Is(err, errNoCertificate) {
		t.Fatalf("Start = %v, want errNoCertificate", err)
	}
}

// Start reports an invalid CIDR allowlist and a failed bind, and does not
// mark the channel running.
func TestStart_Failures(t *testing.T) {
	t.Run("bad cidr", func(t *testing.T) {
		dc := newTestChannel(t, bus.NewMessageBus())
		dc.allowedCIDRs = []string{"not-a-cidr"}
		if err := dc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "allowed_cidrs") {
			t.Fatalf("Start = %v, want an allowed_cidrs error", err)
		}
		if dc.IsRunning() {
			t.Fatal("channel running after a failed start")
		}
	})
	t.Run("bind", func(t *testing.T) {
		rec := bindFreePorts(t)
		rec.failNext = 1
		dc := newTestChannel(t, bus.NewMessageBus())
		if err := dc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("Start = %v, want a listen error", err)
		}
		if dc.IsRunning() {
			t.Fatal("channel running after a failed start")
		}
	})
}

// The default bind seam binds a real loopback listener.
func TestListenTCPDefault(t *testing.T) {
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenTCP: %v", err)
	}
	utils.CloseQuietly(ln)
}

// A stopped channel refuses streaming and replies.
func TestDeviceChannel_NotRunning(t *testing.T) {
	dc := newTestChannel(t, bus.NewMessageBus())
	if err := dc.StreamDelta(context.Background(), "device:dev1", "", "hi"); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("StreamDelta = %v, want ErrNotRunning", err)
	}
	if err := dc.Send(context.Background(), bus.OutboundMessage{ChatID: "device:dev1", Content: "hi"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("Send = %v, want ErrNotRunning", err)
	}
}

// Through a running channel, a streamed delta reaches the device and
// DisconnectDevice closes its connection.
func TestDeviceChannel_StreamAndDisconnect(t *testing.T) {
	dc, addr := startChannel(t, config.DeviceChannelConfig{AutoApprove: true}, nil)
	em := newEmulator(t)
	conn := mustOpen(t, em, "ws://"+addr+"/", "")
	em.writeReq(t, conn, "m1", "chat.send", map[string]any{"message": "hi", "idempotencyKey": "run-1"})
	readRes(t, conn, "m1")

	if err := dc.StreamDelta(context.Background(), "device:"+em.deviceID(), "run-1", "Hel"); err != nil {
		t.Fatalf("StreamDelta: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var f collectedFrame
	if err := conn.ReadJSON(&f); err != nil || f.Event != "chat" || !strings.Contains(string(f.Payload), `"deltaText":"Hel"`) {
		t.Fatalf("frame = %+v (%v), want the chat delta", f, err)
	}

	dc.DisconnectDevice(em.deviceID())
	if code := readUntilClose(t, conn, 5*time.Second); code != 1008 {
		t.Fatalf("close code = %d, want 1008 (policy violation)", code)
	}
}

// Blank allowlist entries are ignored; a client address without a port or
// that is not an IP is judged correctly.
func TestIPAllowlist_Edges(t *testing.T) {
	h, err := ipAllowlistHandler([]string{" ", "10.0.0.0/8"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatalf("ipAllowlistHandler: %v", err)
	}
	for addr, want := range map[string]int{
		"10.1.2.3":         http.StatusNoContent,
		"10.1.2.3:5000":    http.StatusNoContent,
		"127.0.0.1:5000":   http.StatusNoContent,
		"203.0.113.7:5000": http.StatusForbidden,
		"not-an-ip":        http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("client %q: status %d, want %d", addr, rr.Code, want)
		}
	}
}

// The registered factory refuses an invalid lockout_exempt or
// trusted_proxies entry, and a data directory it cannot use, by building no
// channel.
func TestDeviceFactory_Failures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cfg *config.Config)
	}{
		{"lockout_exempt", func(_ *testing.T, cfg *config.Config) { cfg.Gateway.LockoutExempt = []string{"not-an-ip"} }},
		{"trusted_proxies", func(_ *testing.T, cfg *config.Config) { cfg.Gateway.TrustedProxies = []string{"not-an-ip"} }},
		{"data dir", func(t *testing.T, cfg *config.Config) {
			t.Helper()
			if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.DataDir(), global.InternalDir), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(global.EnvVarHome, t.TempDir())
			cfg := config.DefaultConfig()
			cfg.Channels.Device.Enabled = true
			cfg.Channels.Device.Token = "shared"
			tc.setup(t, cfg)
			cm, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if ch, ok := cm.Channel("device"); ok {
				if dc, isDC := ch.(*DeviceChannel); isDC {
					utils.CloseQuietly(dc.store)
				}
				t.Fatal("device channel built from an invalid configuration")
			}
		})
	}
}

// LANIPv4s lists only IPv4 addresses that are not loopback, link-local or
// the Docker default bridge.
func TestLANIPv4s(t *testing.T) {
	for _, s := range LANIPv4s() {
		ip := net.ParseIP(s).To4()
		if ip == nil {
			t.Fatalf("%q is not an IPv4 address", s)
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || (ip[0] == 172 && ip[1] == 17) {
			t.Fatalf("%q should have been excluded", s)
		}
	}
}

func TestIsLoopbackHost_Names(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.2": true, "10.0.0.1": false, "example.com": false, "::1": true} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// An external URL that names no host is refused.
func TestBuildSetupPayload_InvalidExternalURL(t *testing.T) {
	if _, err := BuildSetupPayload("http://", "", nil, 18791, "tok", false); err == nil {
		t.Fatal("BuildSetupPayload with a host-less external URL: want an error")
	}
}

// EnsureProvisioned reports a config it cannot load or save.
func TestEnsureProvisioned_Failures(t *testing.T) {
	t.Setenv(global.EnvVarHome, t.TempDir())
	t.Run("load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := EnsureProvisioned(path); err == nil {
			t.Fatal("EnsureProvisioned(corrupt config): want an error")
		}
	})
	t.Run("save", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Errorf("restore mode: %v", err)
			}
		})
		if _, _, err := EnsureProvisioned(path); err == nil {
			t.Fatal("EnsureProvisioned into a read-only directory: want an error")
		}
	})
}
