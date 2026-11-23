package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/routing"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// execEdgeLoop returns a test loop, its default agent and a captured log.
func execEdgeLoop(t *testing.T) (*testLoop, *AgentInstance, *safeBufLoop) {
	t.Helper()
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	tl := newTestAgentLoop(t)
	agent := tl.al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	return tl, agent, logs
}

// execEdgeNextInbound returns the next inbound message, after publishing a
// marker so that "nothing was published" reads as the marker coming first.
func execEdgeNextInbound(t *testing.T, mb *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	if err := mb.PublishInbound(context.Background(), bus.InboundMessage{Content: "execcov-marker"}); err != nil {
		t.Fatalf("publish marker: %v", err)
	}
	msg, ok := mb.ConsumeInbound(context.Background())
	if !ok {
		t.Fatal("inbound bus closed")
	}
	if msg.Content != "execcov-marker" {
		if m, ok := mb.ConsumeInbound(context.Background()); !ok || m.Content != "execcov-marker" {
			t.Fatalf("expected marker after %q, got %q", msg.Content, m.Content)
		}
	}
	return msg
}

// execEdgeNextOutbound is execEdgeNextInbound for the outbound queue.
func execEdgeNextOutbound(t *testing.T, mb *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	if err := mb.PublishOutbound(context.Background(), bus.OutboundMessage{Content: "execcov-marker"}); err != nil {
		t.Fatalf("publish marker: %v", err)
	}
	msg, ok := mb.SubscribeOutbound(context.Background())
	if !ok {
		t.Fatal("outbound bus closed")
	}
	if msg.Content != "execcov-marker" {
		if m, ok := mb.SubscribeOutbound(context.Background()); !ok || m.Content != "execcov-marker" {
			t.Fatalf("expected marker after %q, got %q", msg.Content, m.Content)
		}
	}
	return msg
}

func TestToolExec_AsyncCallbackDeliversUserAndModelOutput(t *testing.T) {
	tl, agent, _ := execEdgeLoop(t)
	opts := processOptions{Channel: "telegram", ChatID: "c1", SessionKey: "s1"}
	ctx := toolsagents.WithSpawnDepth(context.Background(), 2)

	cb := tl.al.asyncToolCallback(ctx, agent, opts, "slow_tool")
	cb(context.Background(), &toolkit.Result{ForUser: "for you", ForLLM: "for the model"})

	out := execEdgeNextOutbound(t, tl.msgBus)
	if out.Channel != "telegram" || out.ChatID != "c1" || out.Content != "for you" {
		t.Errorf("outbound = %+v, want the ForUser text in telegram/c1", out)
	}
	in := execEdgeNextInbound(t, tl.msgBus)
	wantAgent, wantSession := asyncResultTarget(agent)
	if in.Channel != "system" || in.SenderID != "async:slow_tool" || !in.Internal ||
		in.ChatID != "c1" || in.Content != "for the model" || in.SessionKey != wantSession {
		t.Errorf("inbound = %+v", in)
	}
	if in.SessionKey != routing.BuildAgentMainSessionKey(wantAgent) {
		t.Errorf("session key %q is not the agent's main session", in.SessionKey)
	}
	if got := in.Metadata[metadataKeyPreresolvedAgentID]; got != wantAgent {
		t.Errorf("preresolved agent = %q, want %q", got, wantAgent)
	}
	if got := in.Metadata[bus.MetaOriginChannel]; got != "telegram" {
		t.Errorf("origin channel = %q, want telegram", got)
	}
	if got := in.Metadata[bus.MetaSpawnDepth]; got != "2" {
		t.Errorf("spawn depth = %q, want 2", got)
	}
}

func TestToolExec_AsyncCallbackErrorOnly(t *testing.T) {
	tl, agent, _ := execEdgeLoop(t)
	opts := processOptions{Channel: constants.AgentMessageChannel, ChatID: "c1", SessionKey: "s1"}

	cb := tl.al.asyncToolCallback(context.Background(), agent, opts, "slow_tool")
	cb(context.Background(), &toolkit.Result{ForUser: "dropped", Err: errors.New("tool broke"), IsError: true})

	// An asked turn has no chat: nothing goes out.
	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "execcov-marker" {
		t.Errorf("unexpected outbound %q for an asked turn", out.Content)
	}
	in := execEdgeNextInbound(t, tl.msgBus)
	if !strings.Contains(in.Content, "tool broke") {
		t.Errorf("inbound content = %q, want the error text", in.Content)
	}
	if _, ok := in.Metadata[bus.MetaSpawnDepth]; ok {
		t.Errorf("spawn depth set for a depth-0 turn: %v", in.Metadata)
	}
}

func TestToolExec_AsyncCallbackNothingToSay(t *testing.T) {
	tl, agent, _ := execEdgeLoop(t)
	opts := processOptions{Channel: "telegram", ChatID: "c1", SessionKey: "s1"}

	cb := tl.al.asyncToolCallback(context.Background(), agent, opts, "slow_tool")
	cb(context.Background(), &toolkit.Result{ForUser: "hidden", Silent: true})

	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "execcov-marker" {
		t.Errorf("silent result reached the user: %q", out.Content)
	}
	if in := execEdgeNextInbound(t, tl.msgBus); in.Content != "execcov-marker" {
		t.Errorf("empty result re-entered the agent: %q", in.Content)
	}
}

func TestToolExec_PublishFailuresAreLogged(t *testing.T) {
	tl, agent, logs := execEdgeLoop(t)
	opts := processOptions{Channel: "telegram", ChatID: "c1", SessionKey: "s1"}
	tl.msgBus.Close()
	ctx := context.Background()

	tl.al.asyncToolCallback(ctx, agent, opts, "slow_tool")(ctx, &toolkit.Result{ForUser: "u", ForLLM: "m"})
	tl.al.publishBreadcrumb(ctx, opts, spawnllm.ToolCall{Name: "file_list"})
	tl.al.publishToolForUser(ctx, opts, "t", &toolkit.Result{ForUser: "u"})
	tl.al.publishToolMedia(ctx, opts, "t", &toolkit.Result{Media: []string{"media://x"}})

	for _, want := range []string{
		"Failed to deliver async tool result to user",
		"Failed to deliver async tool result to agent",
		"Failed to publish tool breadcrumb",
		"Failed to publish tool ForUser content",
		"Failed to publish tool media",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

func TestToolExec_BreadcrumbAndForUser(t *testing.T) {
	tl, _, _ := execEdgeLoop(t)
	opts := processOptions{Channel: "telegram", ChatID: "c1"}
	ctx := context.Background()

	// A call with no name has no breadcrumb.
	tl.al.publishBreadcrumb(ctx, opts, spawnllm.ToolCall{})
	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "execcov-marker" {
		t.Errorf("nameless call posted %q", out.Content)
	}

	tl.al.publishBreadcrumb(ctx, opts, spawnllm.ToolCall{Name: "file_list", Arguments: map[string]any{"path": "a/b"}})
	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "🔧 `file_list` `b`" || out.ChatID != "c1" {
		t.Errorf("breadcrumb = %+v", out)
	}

	tl.al.publishToolForUser(ctx, opts, "t", &toolkit.Result{ForUser: "hello"})
	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "hello" || out.Channel != "telegram" {
		t.Errorf("ForUser = %+v", out)
	}
}

func TestToolExec_MediaCarriesStoredMeta(t *testing.T) {
	tl, _, _ := execEdgeLoop(t)
	store := media.NewFileMediaStore()
	tl.al.SetMediaStore(store)
	path := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(path, media.MediaMeta{Filename: "pic.png", ContentType: "image/png"}, "scope")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	opts := processOptions{Channel: "telegram", ChatID: "c1"}

	tl.al.publishToolMedia(context.Background(), opts, "t", &toolkit.Result{Media: []string{ref, "media://unknown"}})

	msg, ok := tl.msgBus.SubscribeOutboundMedia(context.Background())
	if !ok {
		t.Fatal("media bus closed")
	}
	if msg.Channel != "telegram" || msg.ChatID != "c1" || len(msg.Parts) != 2 {
		t.Fatalf("media message = %+v", msg)
	}
	if p := msg.Parts[0]; p.Ref != ref || p.Filename != "pic.png" || p.ContentType != "image/png" || p.Type != "image" {
		t.Errorf("known part = %+v", p)
	}
	if p := msg.Parts[1]; p.Ref != "media://unknown" || p.Filename != "" || p.Type != "" {
		t.Errorf("unknown part = %+v", p)
	}
}
