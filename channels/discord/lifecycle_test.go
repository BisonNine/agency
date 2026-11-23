package discord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
)

// NewDiscordChannel builds a stopped channel with the bot token, Discord's
// message limit and the configured group trigger; a bad proxy is refused.
func TestNewDiscordChannel(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	cfg := config.DiscordConfig{
		Token:              "secret",
		AllowFrom:          config.FlexibleStringSlice{"u1"},
		GroupTrigger:       config.GroupTriggerConfig{MentionOnly: true},
		ReasoningChannelID: "r1",
	}
	c, err := NewDiscordChannel(cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewDiscordChannel = %v", err)
	}
	if c.Name() != "discord" || c.IsRunning() {
		t.Errorf("name %q running %v, want a stopped discord channel", c.Name(), c.IsRunning())
	}
	if c.session.Token != "Bot secret" {
		t.Errorf("token = %q, want %q", c.session.Token, "Bot secret")
	}
	if c.MaxMessageLength() != 2000 || c.ReasoningChannelID() != "r1" {
		t.Errorf("max %d reasoning %q, want 2000 and r1", c.MaxMessageLength(), c.ReasoningChannelID())
	}
	if !c.IsAllowed("u1") || c.IsAllowed("u2") {
		t.Error("allow list not applied")
	}
	if ok, _ := c.ShouldRespondInGroup(false, "hi"); ok {
		t.Error("group trigger not applied")
	}

	cfg.Proxy = "://bad"
	if _, err := NewDiscordChannel(cfg, bus.NewMessageBus()); err == nil {
		t.Error("bad proxy: nil error")
	}
}

// Start looks up the bot user, opens the gateway and marks the channel
// running.
func TestStart_Succeeds(t *testing.T) {
	newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})
	c.SetRunning(false)
	c.botUserID = ""
	opened := false
	c.openSession = func() error { opened = true; return nil }

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v", err)
	}
	if !opened || !c.IsRunning() {
		t.Fatalf("opened %v running %v, want both", opened, c.IsRunning())
	}
	if c.botUserID != "bot" {
		t.Errorf("bot user = %q, want bot", c.botUserID)
	}
}

// A failed bot-user lookup or gateway open fails the start.
func TestStart_Failures(t *testing.T) {
	t.Run("bot user", func(t *testing.T) {
		f := newFakeDiscord(t)
		f.userFail = true
		c, _ := newFakeChannel(t, config.DiscordConfig{})
		c.SetRunning(false)
		if err := c.Start(context.Background()); err == nil {
			t.Fatal("Start = nil, want an error")
		}
		if c.IsRunning() {
			t.Error("running after a failed start")
		}
	})
	t.Run("gateway", func(t *testing.T) {
		newFakeDiscord(t)
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(gw.Close)
		orig := discordgo.EndpointGateway
		discordgo.EndpointGateway = gw.URL + "/gateway"
		t.Cleanup(func() { discordgo.EndpointGateway = orig })

		c, _ := newFakeChannel(t, config.DiscordConfig{})
		c.SetRunning(false)
		if err := c.Start(context.Background()); err == nil {
			t.Fatal("Start = nil, want an error")
		}
		if c.IsRunning() {
			t.Error("running after a failed open")
		}
	})
}

// A session that opens after its start was abandoned is closed even when the
// close fails (the failure is logged).
func TestStart_AbandonedOpenCloseFails(t *testing.T) {
	newFakeDiscord(t)
	c, _ := newFakeChannel(t, config.DiscordConfig{})
	c.SetRunning(false)
	opening := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{}, 1)
	c.openSession = func() error {
		close(opening)
		<-release
		return nil
	}
	c.closeLate = func() error {
		closed <- struct{}{}
		return errors.New("close failed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	<-opening
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context.Canceled", err)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("late session not closed")
	}
}

// Without the test hook, closeLateSession closes the session itself;
// closing a session that never connected succeeds.
func TestSessionHooksDefault(t *testing.T) {
	session, err := discordgo.New("Bot x")
	if err != nil {
		t.Fatal(err)
	}
	c := &DiscordChannel{session: session}
	if err := c.closeLateSession(); err != nil {
		t.Errorf("closeLateSession = %v", err)
	}
}

// Without a bot user id there is no mention to strip.
func TestStripBotMention_NoBotID(t *testing.T) {
	c := &DiscordChannel{}
	if got := c.stripBotMention(" <@1> hi "); got != " <@1> hi " {
		t.Errorf("stripBotMention = %q, want the text unchanged", got)
	}
}

// A proxy is applied to a session that has no websocket dialer yet.
func TestApplyDiscordProxy_NoDialer(t *testing.T) {
	session, err := discordgo.New("Bot x")
	if err != nil {
		t.Fatal(err)
	}
	session.Dialer = nil
	if err = applyDiscordProxy(session, "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("applyDiscordProxy = %v", err)
	}
	if session.Dialer == nil || session.Dialer.Proxy == nil {
		t.Fatal("no proxied dialer set")
	}
	req, err := http.NewRequest(http.MethodGet, "https://discord.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := session.Dialer.Proxy(req)
	if err != nil || u.String() != "http://127.0.0.1:7890" {
		t.Errorf("dialer proxy = %v, %v; want http://127.0.0.1:7890", u, err)
	}
}
