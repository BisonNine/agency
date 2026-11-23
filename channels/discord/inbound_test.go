package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/utils"
)

// nextInbound returns the next message the channel published.
func nextInbound(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, ok := mb.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("no inbound message was published")
	}
	return msg
}

// marker is a message the channel always publishes; tests send it after one
// that must be dropped, so the next message on the bus proves the drop.
func marker(id string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: id, ChannelID: "dm", Content: "marker", Author: &discordgo.User{ID: "u1", Username: "alice"},
	}}
}

// A DM reaches the agents with the bot mention removed, the sender's tag as
// display name, and the sender as the peer.
func TestHandleMessage_DirectMessage(t *testing.T) {
	newFakeDiscord(t)
	c, mb := newFakeChannel(t, config.DiscordConfig{})

	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "m1", ChannelID: "dm1", Content: "<@bot> hello <@!bot>",
		Author: &discordgo.User{ID: "u1", Username: "alice", Discriminator: "1234"},
	}})

	msg := nextInbound(t, mb)
	if msg.Content != "hello" {
		t.Errorf("content = %q, want %q", msg.Content, "hello")
	}
	if msg.Peer.Kind != "direct" || msg.Peer.ID != "u1" {
		t.Errorf("peer = %+v, want direct u1", msg.Peer)
	}
	if msg.ChatID != "dm1" || msg.MessageID != "m1" {
		t.Errorf("chat/message = %q/%q, want dm1/m1", msg.ChatID, msg.MessageID)
	}
	want := map[string]string{
		"user_id": "u1", "username": "alice", "display_name": "alice#1234",
		"guild_id": "", "channel_id": "dm1", "is_dm": "true",
	}
	for k, v := range want {
		if msg.Metadata[k] != v {
			t.Errorf("metadata[%s] = %q, want %q", k, msg.Metadata[k], v)
		}
	}
	if msg.Sender.DisplayName != "alice#1234" || msg.Sender.CanonicalID == "" {
		t.Errorf("sender = %+v, want display alice#1234 and a canonical id", msg.Sender)
	}
}

// Messages the channel must not hand on: none, no author, the bot's own,
// a sender not allowed, and one left empty.
func TestHandleMessage_Dropped(t *testing.T) {
	newFakeDiscord(t)
	c, mb := newFakeChannel(t, config.DiscordConfig{AllowFrom: config.FlexibleStringSlice{"u1"}})

	c.handleMessage(c.session, nil)
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{ID: "x1", Content: "no author"}})
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "x2", ChannelID: "dm", Content: "mine", Author: &discordgo.User{ID: "bot"},
	}})
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "x3", ChannelID: "dm", Content: "stranger", Author: &discordgo.User{ID: "u2", Username: "bob"},
	}})
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "x4", ChannelID: "dm", Content: "<@bot>", Author: &discordgo.User{ID: "u1", Username: "alice"},
	}})
	c.handleMessage(c.session, marker("ok"))

	if msg := nextInbound(t, mb); msg.MessageID != "ok" {
		t.Fatalf("published %q (%q), want only the marker", msg.MessageID, msg.Content)
	}
}

// In a server channel the group trigger decides: with mention_only, a message
// naming the bot is answered and one that does not is ignored.
func TestHandleMessage_GuildMentionOnly(t *testing.T) {
	newFakeDiscord(t)
	c, mb := newFakeChannel(t, config.DiscordConfig{GroupTrigger: config.GroupTriggerConfig{MentionOnly: true}})

	author := &discordgo.User{ID: "u1", Username: "alice", Discriminator: "0"}
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "g1", ChannelID: "c1", GuildID: "g", Content: "not for you", Author: author,
		Mentions: []*discordgo.User{{ID: "someone-else"}},
	}})
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "g2", ChannelID: "c1", GuildID: "g", Content: "<@bot>  hi there ", Author: author,
		Mentions: []*discordgo.User{{ID: "someone-else"}, {ID: "bot"}},
	}})

	msg := nextInbound(t, mb)
	if msg.MessageID != "g2" {
		t.Fatalf("published %q, want g2 only", msg.MessageID)
	}
	if msg.Content != "hi there" {
		t.Errorf("content = %q, want %q", msg.Content, "hi there")
	}
	if msg.Peer.Kind != "channel" || msg.Peer.ID != "c1" {
		t.Errorf("peer = %+v, want channel c1", msg.Peer)
	}
	if msg.Metadata["is_dm"] != "false" || msg.Metadata["guild_id"] != "g" || msg.Metadata["display_name"] != "alice" {
		t.Errorf("metadata = %v, want a guild message from alice", msg.Metadata)
	}
}

// Channel references become names (from the state cache, else the API), and
// links to messages in the same server are expanded; others are left alone.
func TestHandleMessage_ResolvesRefsAndLinks(t *testing.T) {
	f := newFakeDiscord(t)
	f.channels["222"] = "general"
	f.messages["300/301"] = &discordgo.Message{ID: "301", Content: "linked text", Author: &discordgo.User{ID: "u9", Username: "bob"}}
	f.messages["300/302"] = &discordgo.Message{ID: "302", Content: "no author"}
	f.messages["300/303"] = &discordgo.Message{ID: "303"}
	c, mb := newFakeChannel(t, config.DiscordConfig{})
	if err := c.session.State.GuildAdd(&discordgo.Guild{ID: "5"}); err != nil {
		t.Fatal(err)
	}
	if err := c.session.State.ChannelAdd(&discordgo.Channel{ID: "111", GuildID: "5", Name: "cached"}); err != nil {
		t.Fatal(err)
	}

	content := "see <#111> <#222> <#999> " +
		"https://discord.com/channels/5/300/301 " +
		"https://discord.com/channels/6/300/301 " +
		"https://discord.com/channels/5/300/302"
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "r1", ChannelID: "c1", GuildID: "5", Content: content, Author: &discordgo.User{ID: "u1", Username: "alice"},
	}})

	got := nextInbound(t, mb).Content
	for _, want := range []string{
		"see #cached #general <#999> ",
		"\n[linked message from bob]: linked text",
		"\n[linked message from unknown]: no author",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("content = %q, want it to contain %q", got, want)
		}
	}
	if strings.Count(got, "[linked message") != 2 {
		t.Errorf("content = %q, want exactly the two same-server links expanded", got)
	}

	// Missing or empty linked messages add nothing.
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "r2", ChannelID: "c1", GuildID: "5", Author: &discordgo.User{ID: "u1", Username: "alice"},
		Content: "https://discord.com/channels/5/300/303 https://discord.com/channels/5/300/404",
	}})
	if got := nextInbound(t, mb).Content; strings.Contains(got, "[linked message") {
		t.Errorf("content = %q, want no expansion", got)
	}
}

// A reply carries the quoted message, named by its author ("unknown" when
// Discord does not say); an empty quote is left out.
func TestHandleMessage_QuotedReply(t *testing.T) {
	newFakeDiscord(t)
	c, mb := newFakeChannel(t, config.DiscordConfig{})
	author := &discordgo.User{ID: "u1", Username: "alice"}
	ref := &discordgo.MessageReference{MessageID: "q"}

	tests := []struct {
		id     string
		quoted *discordgo.Message
		want   string
	}{
		{"q1", &discordgo.Message{Content: "earlier", Author: &discordgo.User{Username: "bob"}}, "[quoted message from bob]: earlier\n\nnow"},
		{"q2", &discordgo.Message{Content: "earlier"}, "[quoted message from unknown]: earlier\n\nnow"},
		{"q3", &discordgo.Message{}, "now"},
	}
	for _, tt := range tests {
		c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
			ID: tt.id, ChannelID: "dm", Content: "now", Author: author,
			MessageReference: ref, ReferencedMessage: tt.quoted,
		}})
		if got := nextInbound(t, mb).Content; got != tt.want {
			t.Errorf("%s: content = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// Attachments: a file is passed on by URL; an audio file is downloaded and
// registered with the media store; an audio file that cannot be downloaded
// falls back to its URL.
func TestHandleMessage_Attachments(t *testing.T) {
	utils.SetMediaStagingDir(t.TempDir(), 0, 0)
	t.Cleanup(func() { utils.SetMediaStagingDir("", 0, 0) })
	files := http.NewServeMux()
	files.HandleFunc("/voice.ogg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		if _, err := w.Write([]byte("OggS")); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	srv := httptest.NewServer(files)
	t.Cleanup(srv.Close)

	newFakeDiscord(t)
	c, mb := newFakeChannel(t, config.DiscordConfig{})
	store := media.NewFileMediaStore()
	c.SetMediaStore(store)
	author := &discordgo.User{ID: "u1", Username: "alice"}

	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "a1", ChannelID: "dm", Author: author,
		Attachments: []*discordgo.MessageAttachment{
			{URL: srv.URL + "/doc.pdf", Filename: "doc.pdf", ContentType: "application/pdf"},
			{URL: srv.URL + "/voice.ogg", Filename: "voice.ogg"},
			{URL: srv.URL + "/gone.mp3", Filename: "gone.mp3"},
		},
	}})

	msg := nextInbound(t, mb)
	wantContent := "[attachment: " + srv.URL + "/doc.pdf]\n[audio: voice.ogg]\n[attachment: " + srv.URL + "/gone.mp3]"
	if msg.Content != wantContent {
		t.Errorf("content = %q, want %q", msg.Content, wantContent)
	}
	if len(msg.Media) != 3 || msg.Media[0] != srv.URL+"/doc.pdf" || msg.Media[2] != srv.URL+"/gone.mp3" {
		t.Fatalf("media = %v, want the pdf URL, a stored ref and the mp3 URL", msg.Media)
	}
	path, err := store.Resolve(msg.Media[1])
	if err != nil {
		t.Fatalf("audio ref %q: %v", msg.Media[1], err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "OggS" {
		t.Errorf("downloaded audio = %q, %v; want OggS", body, err)
	}

	// Without a media store the downloaded file's path is passed on.
	c.SetMediaStore(nil)
	c.handleMessage(c.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "a2", ChannelID: "dm", Author: author,
		Attachments: []*discordgo.MessageAttachment{{URL: srv.URL + "/voice.ogg", Filename: "voice.ogg"}},
	}})
	msg = nextInbound(t, mb)
	if len(msg.Media) != 1 || !strings.HasSuffix(msg.Media[0], "voice.ogg") {
		t.Fatalf("media = %v, want the local path of the download", msg.Media)
	}
	if _, err := os.Stat(msg.Media[0]); err != nil {
		t.Errorf("downloaded file: %v", err)
	}
	if msg.Content != "[audio: voice.ogg]" {
		t.Errorf("content = %q, want the audio marker", msg.Content)
	}
}
