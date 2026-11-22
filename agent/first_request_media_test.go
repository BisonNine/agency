// ClawEh
// License: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/ctxengine"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
	"go.bison9.com/agency/providers"
)

// pinRecordingStore is a media store that records the pins its owners set.
type pinRecordingStore struct {
	*media.FileMediaStore
	mu       sync.Mutex
	pins     [][]string
	released []string
}

func (s *pinRecordingStore) ReleasePins(owner string) {
	s.mu.Lock()
	s.released = append(s.released, owner)
	s.mu.Unlock()
	s.FileMediaStore.ReleasePins(owner)
}

func (s *pinRecordingStore) SetPins(owner string, refs []string) {
	s.mu.Lock()
	s.pins = append(s.pins, slices.Clone(refs))
	s.mu.Unlock()
	s.FileMediaStore.SetPins(owner, refs)
}

// compactingFirstIteration is a context manager whose turn starts with
// before, and whose first assembly of the model loop compacts it to after.
type compactingFirstIteration struct {
	capturingContextManager
	before, after []providers.Message
	assembles     int
}

func (c *compactingFirstIteration) Assemble(context.Context, ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	c.assembles++
	if c.assembles == 1 {
		return ctxengine.Assembly{Messages: c.before}, nil
	}
	return ctxengine.Assembly{Messages: c.after, Compacted: true}, nil
}

// messagesProvider records the messages of every call and answers "done".
type messagesProvider struct {
	mu    sync.Mutex
	calls [][]providers.Message
}

func (p *messagesProvider) Chat(_ context.Context, _ providers.Identity, msgs []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(msgs))
	return &providers.LLMResponse{Content: "done", FinishReason: "stop", Normal: true}, nil
}

func (p *messagesProvider) GetDefaultModel() string { return "test-model" }

// When the first assembly of a turn's model loop compacts the history, the
// first model call carries the compacted request's media resolved (an image
// as a data URL, never a media:// reference), and the session's pins follow
// the compacted request: a reference compacted away is released.
func TestFirstRequest_MediaResolvedAfterIterationOneCompaction(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	cfg.Models[0].Vision = config.VisionUserMessage // a model that sees images
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	store := &pinRecordingStore{FileMediaStore: media.NewFileMediaStore()}
	al.mediaStore = store
	dir := t.TempDir()
	ref := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := store.Store(path, media.MediaMeta{Filename: name, ContentType: "image/png"}, "s")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	gone, kept := ref("alice-old.png"), ref("alice-new.png")

	agent := al.registry.Default()
	p := &messagesProvider{}
	setTestProvider(agent, p)
	cm := &compactingFirstIteration{
		before: []providers.Message{{Role: "user", Content: "old"}, {Role: "user", Content: "look", Media: []string{gone, kept}}},
		after:  []providers.Message{{Role: "user", Content: "summary"}, {Role: "user", Content: "look", Media: []string{kept}}},
	}
	entry := &cmEntry{cm: cm, sessionKey: "s"}
	entry.touch()
	al.contextManagers.Store(agent.ID+":s", entry)

	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{SessionKey: "s", Channel: "cli", ChatID: "direct", UserMessage: "look"}); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) == 0 {
		t.Fatal("no model call")
	}
	var media []string
	for _, m := range p.calls[0] {
		media = append(media, m.Media...)
	}
	if len(media) != 1 || !strings.HasPrefix(media[0], "data:image/png;base64,") {
		t.Fatalf("first call media = %v, want the kept image resolved", media)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.pins) == 0 || !slices.Equal(store.pins[len(store.pins)-1], []string{kept}) {
		t.Fatalf("pins set = %v, want only %s", store.pins, kept)
	}
}

// resetPanics is a context manager whose reset panics.
type resetPanics struct{ capturingContextManager }

func (*resetPanics) Reset(context.Context) error { panic("Bob's bug") }

// A session_clear turn resets the session before it releases the session's
// pins and reissues its token: a reset that panics leaves both as they were.
func TestSessionClear_ResetFirst(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, nil)
	store := &pinRecordingStore{FileMediaStore: media.NewFileMediaStore()}
	al.mediaStore = store
	agent := al.registry.Default()
	al.SetSessionTokenIssuer(&issuingSTI{})
	entry := &cmEntry{cm: &resetPanics{}, sessionKey: "s"}
	entry.touch()
	entry.setToken("SST-alice")
	al.contextManagers.Store(agent.ID+":s", entry)
	token := al.sessionToken(agent, "s")
	if token == "" {
		t.Fatal("the session has no token to keep")
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the reset's panic did not reach the caller")
			}
		}()
		_, err := al.runAgentLoop(context.Background(), agent, processOptions{SessionKey: "s", Channel: "cli", ChatID: "direct", UserMessage: "hi", ResetSession: true})
		t.Fatalf("runAgentLoop returned %v instead of panicking", err)
	}()
	if len(store.released) != 0 || al.sessionToken(agent, "s") != token {
		t.Fatalf("released %v, token changed %v", store.released, al.sessionToken(agent, "s") != token)
	}
}
