// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/forum"
	"go.bison9.com/agency/logger"
)

// forumCovModel answers a message containing "silence" with the
// no-response sentinel, one
// containing "explode" with an error, and anything else with "ok".
type forumCovModel struct{}

func (forumCovModel) GetDefaultModel() string { return "mock-model" }

func (forumCovModel) Chat(_ context.Context, _ accounting.Identity, messages []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	user := lastUser(messages)
	switch {
	case strings.Contains(user, "explode"):
		return nil, errors.New("401 unauthorized")
	case strings.Contains(user, "silence"):
		return &spawnllm.LLMResponse{Content: noResponseSentinel}, nil
	}
	return &spawnllm.LLMResponse{Content: "ok"}, nil
}

// Before Bind every method refuses or answers empty, and the modes are the
// defaults.
func TestForumHost_Unbound(t *testing.T) {
	h := NewForumHost()
	ctx := context.Background()
	if d, f := h.Modes("/x"); d != config.DefaultDirMode || f != config.DefaultFileMode {
		t.Errorf("Modes before Bind = %v, %v", d, f)
	}
	if h.Scopes() != nil {
		t.Error("Scopes before Bind is not empty")
	}
	if _, err := h.MayTarget(ctx, "alice", "bob"); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("MayTarget = %v", err)
	}
	if _, err := h.Models(ctx, "alice"); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Models = %v", err)
	}
	if m, d := h.Cooldown("alice"); m != "" || d != 0 {
		t.Errorf("Cooldown = %q, %v", m, d)
	}
	if _, err := h.CreateClone(ctx, forum.CloneSpec{Source: "bob", Owner: "alice"}); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("CreateClone = %v", err)
	}
	if _, err := h.CreateFresh(ctx, forum.FreshSpec{Owner: "alice"}); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("CreateFresh = %v", err)
	}
	if _, err := h.Render(ctx, "card"); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Render = %v", err)
	}
	if err := h.Delete(ctx, "alice", "bob"); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Delete = %v", err)
	}
	if err := h.Touch(ctx, "alice", "bob"); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Touch = %v", err)
	}
	if err := h.ForumFinished(ctx, forum.Origin{AgentID: "alice"}, forum.Chat{}, &forum.Result{}); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("ForumFinished = %v", err)
	}
	h.OnStuck("f1", 1, forum.Origin{AgentID: "alice"}, errors.New("x")) // no loop: nothing to alert, no panic
}

// A loop with a registry but no configuration lists no scopes and answers
// Ask as shut down; the modes are the defaults.
func TestForumHost_NoConfig(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	r := newForumRig(t, forumConfig(t, true, false), &forumModel{})
	h := NewForumHost()
	h.Bind(&AgentLoop{registry: r.al.GetRegistry()})
	if h.Scopes() != nil {
		t.Error("Scopes without a configuration is not empty")
	}
	if d, f := h.Modes("/x"); d != config.DefaultDirMode || f != config.DefaultFileMode {
		t.Errorf("Modes without a configuration = %v, %v", d, f)
	}
	if _, err := h.Ask(context.Background(), "bob", "hi", time.Second); !errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Ask without a configuration = %v", err)
	}
	if info := modelInfo(nil, "alpha"); info != (forum.ModelInfo{Name: "alpha"}) {
		t.Errorf("modelInfo without a configuration = %+v", info)
	}
	if info := modelInfo(r.al.GetConfig(), "ghost"); info != (forum.ModelInfo{Name: "ghost"}) {
		t.Errorf("modelInfo of an unknown model = %+v", info)
	}
}

// On a bound loop: the modes are the agents area's; unknown agents and
// models are refused; Touch of an unknown agent fails; a launcher that is
// gone is named by its id.
func TestForumHost_BoundEdges(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := forumConfig(t, true, false)
	r := newForumRig(t, cfg, &forumModel{})
	h := r.host
	ctx := context.Background()

	alice, _ := r.al.GetRegistry().GetConfigured("alice")
	base := filepath.Join(alice.Workspace, "forums")
	want := cfg.AgentModes(base)
	if d, f := h.Modes(base); d != want.Dir || f != want.File {
		t.Errorf("Modes(%s) = %v, %v; want %v, %v", base, d, f, want.Dir, want.File)
	}
	if _, err := h.Models(ctx, "ghost"); !errors.Is(err, agentreg.ErrNotFound) {
		t.Errorf("Models(ghost) = %v", err)
	}
	if _, err := h.CreateClone(ctx, forum.CloneSpec{Source: "bob", Model: "omega", Owner: "alice"}); err == nil || err.Error() != `model "omega" is not one of agent "bob"'s models` {
		t.Errorf("CreateClone on a model bob lacks = %v", err)
	}
	if err := h.Touch(ctx, "alice", "ghost"); err == nil {
		t.Error("Touch of an unknown agent succeeded")
	}
	gone := forum.WithAskInfo(ctx, forum.AskInfo{ForumID: "f1", Origin: forum.Origin{AgentID: "ghost"}})
	if got := h.sender(gone); got != "ghost" {
		t.Errorf("sender for a launcher that is gone = %q", got)
	}
	if err := h.ForumFinished(ctx, forum.Origin{AgentID: "alice"}, forum.Chat{}, nil); err == nil || err.Error() != "no result" {
		t.Errorf("ForumFinished with no result = %v", err)
	}
}

// An agent with no models has no cooldown to report.
func TestForumHost_CooldownNoModels(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := forumConfig(t, true, false)
	cfg.Agents.Defaults.Models = nil
	r := newForumRig(t, cfg, &forumModel{})
	bob, _ := r.al.GetRegistry().Get("bob")
	if n := len(r.al.chatCandidates(bob)); n != 0 {
		t.Skipf("bob has %d candidate(s) with no models configured", n)
	}
	if m, d := r.host.Cooldown("bob"); m != "" || d != 0 {
		t.Errorf("Cooldown with no models = %q, %v", m, d)
	}
}

// The library unreadable is reported as is, not as a missing card.
func TestForumHost_RenderStoreError(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := forumConfig(t, true, false)
	if err := os.MkdirAll(filepath.Dir(cfg.CardsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.CardsPath(), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newForumRig(t, cfg, &forumModel{})
	if _, err := r.host.Render(context.Background(), "bob-card"); err == nil || errors.Is(err, forum.ErrCardNotFound) {
		t.Errorf("Render with the library a file = %v", err)
	}
}

// A notice the bus can't take is reported naming the agent.
func TestForumHost_NoticeBusClosed(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	b := bus.NewMessageBus()
	b.Close()
	h := NewForumHost()
	h.Bind(&AgentLoop{bus: b})
	err := h.ForumFinished(context.Background(), forum.Origin{AgentID: "alice"}, forum.Chat{}, &forum.Result{ForumID: "f1", Run: 1, Status: "finished"})
	if err == nil || !strings.HasPrefix(err.Error(), "queue the notice for agent alice: ") {
		t.Errorf("ForumFinished on a closed bus = %v", err)
	}
}

// Ask maps the participant's reply: no text is an empty reply, a failed
// turn (last: its model goes into cooldown) an error reply, and a refused ask (an empty message) an error.
func TestForumHost_AskOutcomes(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	r := newForumRig(t, forumConfig(t, true, false), forumCovModel{})
	ctx := forum.WithAskInfo(context.Background(), forum.AskInfo{ForumID: "f1", Origin: forum.Origin{AgentID: "alice"}})

	reply, err := r.host.Ask(ctx, "bob", "hello", 10*time.Second)
	if err != nil || reply.Outcome != forum.OutcomeOK || reply.Text != "ok" {
		t.Errorf("Ask = %+v, %v; want ok", reply, err)
	}
	reply, err = r.host.Ask(ctx, "bob", "silence", 10*time.Second)
	if err != nil || reply.Outcome != forum.OutcomeEmpty {
		t.Errorf("Ask on a silent model = %+v, %v; want an empty reply", reply, err)
	}
	reply, err = r.host.Ask(ctx, "bob", "explode", 10*time.Second)
	if err != nil || reply.Outcome != forum.OutcomeError {
		t.Errorf("Ask on a failing model = %+v, %v; want an error reply", reply, err)
	}
	if _, err := r.host.Ask(ctx, "bob", "  ", 10*time.Second); err == nil || errors.Is(err, forum.ErrShuttingDown) {
		t.Errorf("Ask with an empty message = %v; want the ask's own error", err)
	}
}
