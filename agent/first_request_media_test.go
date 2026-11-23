// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/media"
)

// pinRecordingStore is a media store that records the pins its owners set
// and counts the refs it resolves.
type pinRecordingStore struct {
	*media.FileMediaStore
	mu       sync.Mutex
	pins     [][]string
	released []string
	resolves int
}

func (s *pinRecordingStore) ResolveWithMeta(ref string) (string, media.MediaMeta, error) {
	s.mu.Lock()
	s.resolves++
	s.mu.Unlock()
	return s.FileMediaStore.ResolveWithMeta(ref)
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
	before, after []spawnllm.Message
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
	calls [][]spawnllm.Message
}

func (p *messagesProvider) Chat(_ context.Context, _ accounting.Identity, msgs []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(msgs))
	return &spawnllm.LLMResponse{Content: "done", FinishReason: "stop", Normal: true}, nil
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
		before: []spawnllm.Message{{Role: "user", Content: "old"}, {Role: "user", Content: "look", Media: []string{gone, kept}}},
		after:  []spawnllm.Message{{Role: "user", Content: "summary"}, {Role: "user", Content: "look", Media: []string{kept}}},
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

// resetCounter is a context manager that counts its resets.
type resetCounter struct {
	capturingContextManager
	resets int
}

func (c *resetCounter) Reset(context.Context) error { c.resets++; return nil }

// A session_clear turn resets the session once.
func TestSessionClear_ResetsOnce(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	setTestProvider(agent, &messagesProvider{})
	cm := &resetCounter{}
	entry := &cmEntry{cm: cm, sessionKey: "s"}
	entry.touch()
	al.contextManagers.Store(agent.ID+":s", entry)
	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{SessionKey: "s", Channel: "cli", ChatID: "direct", UserMessage: "hi", ResetSession: true}); err != nil {
		t.Fatal(err)
	}
	if cm.resets != 1 {
		t.Fatalf("reset %d times, want once", cm.resets)
	}
}

// rebuildingSecondStep is a context manager whose turn starts with before,
// kept by the first step's assembly, and whose second step's assembly
// rewrites it to after with rebuild (a compaction or an eviction). With
// rebuild nil every assembly leaves the history unchanged.
type rebuildingSecondStep struct {
	capturingContextManager
	before, after []spawnllm.Message
	rebuild       func(*ctxengine.Assembly)
	assembles     int
}

func (c *rebuildingSecondStep) Assemble(context.Context, ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	c.assembles++
	// The turn's first request, then the first and second steps.
	if c.assembles < 3 || c.rebuild == nil {
		return ctxengine.Assembly{Messages: c.before}, nil
	}
	asm := ctxengine.Assembly{Messages: c.after}
	c.rebuild(&asm)
	return asm, nil
}

// toolThenDoneProvider records the messages of every call; it asks for
// file_write first and answers "done" after.
type toolThenDoneProvider struct {
	messagesProvider
}

func (p *toolThenDoneProvider) Chat(_ context.Context, _ accounting.Identity, msgs []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(msgs))
	if len(p.calls) == 1 {
		return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{toolCallTo("tc-1", "file_write")}}, nil
	}
	return &spawnllm.LLMResponse{Content: "done", FinishReason: "stop", Normal: true}, nil
}

// mediaTurn is a turn of the default agent on session "s" whose history
// holds two stored images, Alice's old one and her new one, run on cm.
type mediaTurn struct {
	store     *pinRecordingStore
	provider  *toolThenDoneProvider
	agent     *AgentInstance
	gone      string
	kept      string
	runWithCM func(cm *rebuildingSecondStep)
	// run runs the turn on any context manager.
	run func(cm ctxengine.ContextManager)
}

func newMediaTurn(t *testing.T) *mediaTurn {
	t.Helper()
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
	agent := al.registry.Default()
	agent.Tools.Register(&noopWriteFile{})
	if agent.Config != nil {
		agent.Config.Tools = []string{"*"}
	}
	mt := &mediaTurn{store: store, provider: &toolThenDoneProvider{}, agent: agent, gone: ref("alice-old.png"), kept: ref("alice-new.png")}
	setTestProvider(agent, mt.provider)
	mt.run = func(cm ctxengine.ContextManager) {
		entry := &cmEntry{cm: cm, sessionKey: "s"}
		entry.touch()
		al.contextManagers.Store(agent.ID+":s", entry)
		if _, err := al.runAgentLoop(context.Background(), agent, processOptions{SessionKey: "s", Channel: "cli", ChatID: "direct", UserMessage: "look"}); err != nil {
			t.Fatal(err)
		}
	}
	mt.runWithCM = func(cm *rebuildingSecondStep) { mt.run(cm) }
	return mt
}

// mediaOf is every media entry of msgs, in order.
func mediaOf(msgs []spawnllm.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Media...)
	}
	return out
}

// When the second step's assembly rewrites the history (a compaction or an
// eviction), the request it rebuilds from the store is resolved again: the
// model gets the image still present as a data URL, never its media://
// reference, and the session's pins follow the rebuilt request, releasing
// the reference that was compacted away.
func TestRequestMedia_ResolvedAfterSecondStepRebuild(t *testing.T) {
	tests := []struct {
		name    string
		rebuild func(*ctxengine.Assembly)
	}{
		{"compaction", func(a *ctxengine.Assembly) { a.Compacted = true }},
		{"eviction", func(a *ctxengine.Assembly) {
			a.Evictions = []ctxengine.EvictionEvent{{Tool: "file_read_bytes", Resource: "notes.txt", Bytes: 10, Reason: "stale"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := newMediaTurn(t)
			cm := &rebuildingSecondStep{
				before:  []spawnllm.Message{{Role: "user", Content: "old", Media: []string{mt.gone}}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
				after:   []spawnllm.Message{{Role: "user", Content: "summary"}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
				rebuild: tt.rebuild,
			}
			mt.runWithCM(cm)

			if cm.assembles != 3 {
				t.Fatalf("assemblies = %d, want 3 (the first request and two steps)", cm.assembles)
			}
			if len(mt.provider.calls) != 2 {
				t.Fatalf("model calls = %d, want 2", len(mt.provider.calls))
			}
			got := mediaOf(mt.provider.calls[1])
			if len(got) != 1 || !strings.HasPrefix(got[0], "data:image/png;base64,") {
				t.Fatalf("second call media = %v, want the kept image resolved", got)
			}
			mt.store.mu.Lock()
			defer mt.store.mu.Unlock()
			if len(mt.store.pins) == 0 || !slices.Equal(mt.store.pins[len(mt.store.pins)-1], []string{mt.kept}) {
				t.Fatalf("pins set = %v, want only %s", mt.store.pins, mt.kept)
			}
			// Both images for the first request, the one still present for
			// the rebuilt one.
			if mt.store.resolves != 3 {
				t.Fatalf("refs resolved = %d, want 3", mt.store.resolves)
			}
		})
	}
}

// On a turn whose history is never rewritten, the media is resolved once,
// for the first request: the second request is the first one with the
// step's messages appended, its images the same data URLs, and the pins
// are set once.
func TestRequestMedia_ResolvedOnceWithoutRebuild(t *testing.T) {
	mt := newMediaTurn(t)
	cm := &rebuildingSecondStep{
		before: []spawnllm.Message{{Role: "user", Content: "old", Media: []string{mt.gone}}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
	}
	mt.runWithCM(cm)

	if cm.assembles != 3 {
		t.Fatalf("assemblies = %d, want 3 (the first request and two steps)", cm.assembles)
	}
	if len(mt.provider.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(mt.provider.calls))
	}
	first, second := mt.provider.calls[0], mt.provider.calls[1]
	if len(second) <= len(first) || !reflect.DeepEqual(second[:len(first)], first) {
		t.Fatalf("second request does not extend the first:\nfirst  %+v\nsecond %+v", first, second)
	}
	for _, m := range mediaOf(first) {
		if !strings.HasPrefix(m, "data:image/png;base64,") {
			t.Fatalf("first call media = %v, want both images resolved", mediaOf(first))
		}
	}
	mt.store.mu.Lock()
	defer mt.store.mu.Unlock()
	if mt.store.resolves != 2 {
		t.Fatalf("refs resolved = %d, want 2 (each image once)", mt.store.resolves)
	}
	if len(mt.store.pins) != 1 || !slices.Equal(mt.store.pins[0], []string{mt.gone, mt.kept}) {
		t.Fatalf("pins set = %v, want both images once", mt.store.pins)
	}
}

// When a step's rebuild removes every attachment, the session's pins follow
// it at once: the rebuilt request pins nothing, so both images are released
// in this turn rather than the next.
func TestRequestMedia_RebuildWithoutAttachmentsUnpins(t *testing.T) {
	tests := []struct {
		name    string
		rebuild func(*ctxengine.Assembly)
	}{
		{"compaction", func(a *ctxengine.Assembly) { a.Compacted = true }},
		{"eviction", func(a *ctxengine.Assembly) {
			a.Evictions = []ctxengine.EvictionEvent{{Tool: "file_read_bytes", Resource: "notes.txt", Bytes: 10, Reason: "stale"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := newMediaTurn(t)
			cm := &rebuildingSecondStep{
				before:  []spawnllm.Message{{Role: "user", Content: "old", Media: []string{mt.gone}}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
				after:   []spawnllm.Message{{Role: "user", Content: "summary"}, {Role: "user", Content: "look"}},
				rebuild: tt.rebuild,
			}
			mt.runWithCM(cm)

			if len(mt.provider.calls) != 2 {
				t.Fatalf("model calls = %d, want 2", len(mt.provider.calls))
			}
			if got := mediaOf(mt.provider.calls[1]); len(got) != 0 {
				t.Fatalf("second call media = %v, want none", got)
			}
			mt.store.mu.Lock()
			defer mt.store.mu.Unlock()
			if len(mt.store.pins) != 2 || !slices.Equal(mt.store.pins[0], []string{mt.gone, mt.kept}) || len(mt.store.pins[1]) != 0 {
				t.Fatalf("pins set = %v, want both images, then none", mt.store.pins)
			}
			if mt.store.resolves != 2 {
				t.Fatalf("refs resolved = %d, want 2 (the first request's)", mt.store.resolves)
			}
		})
	}
}

// overflowingOnce is a context manager whose turn starts with before and
// keeps it, until a forced compression after a context-window error makes
// the next assembly return after.
type overflowingOnce struct {
	capturingContextManager
	before, after []spawnllm.Message
	compressed    bool
}

func (c *overflowingOnce) Assemble(context.Context, ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	if c.compressed {
		return ctxengine.Assembly{Messages: c.after, Compacted: true}, nil
	}
	return ctxengine.Assembly{Messages: c.before}, nil
}

func (c *overflowingOnce) ForceCompress(context.Context) error {
	c.compressed = true
	return nil
}

// overflowThenDoneProvider records the messages of every call; the first
// overflows the context window, the next answers "done".
type overflowThenDoneProvider struct {
	messagesProvider
}

func (p *overflowThenDoneProvider) Chat(_ context.Context, _ accounting.Identity, msgs []spawnllm.Message, _ []spawnllm.ToolDefinition, _ string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(msgs))
	if len(p.calls) == 1 {
		return nil, errors.New("context window exceeded: Alice's request is too long")
	}
	return &spawnllm.LLMResponse{Content: "done", FinishReason: "stop", Normal: true}, nil
}

// When a request overflows the context window, the request the retry rebuilds
// from the compressed store is resolved again and the pins follow it: the
// retry carries the image still present as a data URL, and the reference
// compressed away is released at once.
func TestRequestMedia_ResolvedAfterOverflowRetry(t *testing.T) {
	mt := newMediaTurn(t)
	p := &overflowThenDoneProvider{}
	setTestProvider(mt.agent, p)
	cm := &overflowingOnce{
		before: []spawnllm.Message{{Role: "user", Content: "old", Media: []string{mt.gone}}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
		after:  []spawnllm.Message{{Role: "user", Content: "summary"}, {Role: "user", Content: "look", Media: []string{mt.kept}}},
	}
	mt.run(cm)

	if len(p.calls) != 2 {
		t.Fatalf("model calls = %d, want 2 (the overflow and its retry)", len(p.calls))
	}
	got := mediaOf(p.calls[1])
	if len(got) != 1 || !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Fatalf("retry media = %v, want the kept image resolved", got)
	}
	mt.store.mu.Lock()
	defer mt.store.mu.Unlock()
	if len(mt.store.pins) != 2 || !slices.Equal(mt.store.pins[1], []string{mt.kept}) {
		t.Fatalf("pins set = %v, want both images, then only %s", mt.store.pins, mt.kept)
	}
	if mt.store.resolves != 3 {
		t.Fatalf("refs resolved = %d, want 3", mt.store.resolves)
	}
}
