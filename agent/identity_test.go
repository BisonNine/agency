// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/routing"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// identityRecorder is a provider that answers every call and records the
// identity each call was billed to.
type identityRecorder struct {
	mu  sync.Mutex
	ids []providers.Identity
}

func (r *identityRecorder) Chat(_ context.Context, id providers.Identity, _ []providers.Message,
	_ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (r *identityRecorder) GetDefaultModel() string { return "test-model" }

func (r *identityRecorder) seen() []providers.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]providers.Identity(nil), r.ids...)
}

// TestAgentInstance_Identity: a config agent is billed to itself under its
// display name, a clone to its source under its own label, and a fresh
// temporary agent to its own id under the name it was given. The project is
// always the explicit default.
func TestAgentInstance_Identity(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	reg := newTestAgentLoop(t).al.GetRegistry()
	main, _ := reg.Get("main")
	if got, want := main.Identity(), (global.Identity{Agent: "main", Name: "Main", Project: "default"}); got != want {
		t.Errorf("config agent identity = %+v, want %+v", got, want)
	}

	cloneID, err := reg.CreateClone("main", agentreg.EphemeralMemory())
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)
	want := global.Identity{Agent: "main", Name: "main (clone " + agentreg.ShortID(cloneID) + ")", Project: "default"}
	if got := clone.Identity(); got != want {
		t.Errorf("clone identity = %+v, want %+v", got, want)
	}

	freshID, err := reg.CreateFresh(config.AgentConfig{Name: "Bob"}, agentreg.WithSystemPrompt("You are Bob."))
	if err != nil {
		t.Fatalf("CreateFresh: %v", err)
	}
	fresh, _ := reg.Get(freshID)
	if got, want := fresh.Identity(), (global.Identity{Agent: freshID, Name: "Bob", Project: "default"}); got != want {
		t.Errorf("fresh agent identity = %+v, want %+v", got, want)
	}
}

// TestIdentityOf: a running agent is billed by its Identity, an id the
// registry does not hold by the id itself, and an empty id to no agent.
func TestIdentityOf(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	al := newTestAgentLoop(t).al
	reg := al.GetRegistry()
	main, _ := reg.Get("main")
	cloneID, err := reg.CreateClone("main", agentreg.EphemeralMemory())
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)

	tests := []struct {
		name string
		id   string
		want global.Identity
	}{
		{"config agent", "Main", main.Identity()},
		{"clone", cloneID, clone.Identity()},
		{"unknown", "Alice", global.Identity{Agent: "alice", Name: "Alice", Project: "default"}},
		{"empty", " ", global.NoOwnerIdentity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := al.identityOf(tt.id); got != tt.want {
				t.Errorf("identityOf(%q) = %+v, want %+v", tt.id, got, tt.want)
			}
		})
	}
}

// TestTurn_BillsTheAgent: every model call of a turn carries the turn's agent
// identity; a clone's turn is billed to its source.
func TestTurn_BillsTheAgent(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	al := newTestAgentLoop(t).al
	reg := al.GetRegistry()
	main, _ := reg.Get("main")
	rec := &identityRecorder{}
	main.Provider = rec
	if _, err := al.ProcessDirect(context.Background(), "hello", "direct-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}

	cloneID, err := reg.CreateClone("main", agentreg.EphemeralMemory())
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)
	cloneRec := &identityRecorder{}
	clone.Provider = cloneRec
	key := routing.BuildAgentMainSessionKey(clone.ID)
	if _, err := al.runAgentLoop(context.Background(), clone, processOptions{
		SessionKey: key, Channel: "subagent", ChatID: key, UserMessage: "hello from the clone",
	}); err != nil {
		t.Fatalf("clone turn: %v", err)
	}

	for _, tc := range []struct {
		name string
		rec  *identityRecorder
		want global.Identity
	}{
		{"agent", rec, main.Identity()},
		{"clone", cloneRec, clone.Identity()},
	} {
		ids := tc.rec.seen()
		if len(ids) == 0 {
			t.Fatalf("%s: no model call was made", tc.name)
		}
		for _, id := range ids {
			if id != tc.want {
				t.Errorf("%s: call billed to %+v, want %+v", tc.name, id, tc.want)
			}
		}
	}
	if got := cloneRec.seen()[0].Agent; got != "main" {
		t.Errorf("clone call billed to agent %q, want its source main", got)
	}
}

// TestCompressModelCaller_BillsTheAgent: summaries (and memory
// consolidation, which walks the same chain) are billed to the agent whose
// conversation they summarize.
func TestCompressModelCaller_BillsTheAgent(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	al := newTestAgentLoop(t).al
	main, _ := al.GetRegistry().Get("main")
	caller, _ := al.newCompressModelCaller(main, "agent:main:main")
	if caller.id != main.Identity() {
		t.Fatalf("compression caller identity = %+v, want %+v", caller.id, main.Identity())
	}

	rec := &identityRecorder{}
	cat := compressCatalog(map[string]providers.LLMProvider{"m": rec})
	c := &compressModelCaller{id: main.Identity(), clients: []*providerLLMClient{compressClient(cat, "m")}}
	if _, _, _, err := c.complete(context.Background(), "sys", "user", false, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if ids := rec.seen(); len(ids) != 1 || ids[0] != main.Identity() {
		t.Fatalf("summary calls billed to %+v, want [%+v]", ids, main.Identity())
	}
}

// accountingRecords captures the accounting records written while it is
// installed (the SDK writes them to its logger when no file is open).
type accountingRecords struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (a *accountingRecords) Enabled(context.Context, slog.Level) bool { return true }
func (a *accountingRecords) WithAttrs([]slog.Attr) slog.Handler       { return a }
func (a *accountingRecords) WithGroup(string) slog.Handler            { return a }
func (a *accountingRecords) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(at slog.Attr) bool {
		if at.Key == "accounting" {
			var rec map[string]any
			if err := json.Unmarshal([]byte(at.Value.String()), &rec); err == nil {
				a.mu.Lock()
				a.recs = append(a.recs, rec)
				a.mu.Unlock()
			}
		}
		return true
	})
	return nil
}

func (a *accountingRecords) byKind(kind string) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, r := range a.recs {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

// TestTaskModelHost_BillsTheSpawner: every kind of task-model call (chat,
// image generation and edit, speech, transcription) is billed to the agent
// whose spawner made it; a clone's to its source under the clone's label.
func TestTaskModelHost_BillsTheSpawner(t *testing.T) {
	recs := &accountingRecords{}
	accounting.SetLogger(slog.New(recs))
	t.Cleanup(func() { accounting.SetLogger(nil) })

	e := newTaskHostEnv(t)
	want := global.Identity{Agent: "alice", Name: "alice (clone 1a2b3c4d)", Project: "default"}
	e.host = &taskModelHost{al: e.al, clone: "1a2b3c4d", id: want}
	e.write(t, "files/clip.ogg", []byte("OggS fake"))
	e.write(t, "files/in.png", pngBytes)
	for _, req := range []toolsagents.TaskModelRequest{
		{Kind: "text", Prompt: "hi"},
		{Kind: "image", Model: "imgapi", Prompt: "a dog"},
		{Kind: "image", Model: "imgapi", Prompt: "redder", Files: []string{"files/in.png"}},
		{Kind: "tts", Prompt: "Hello Bob"},
		{Kind: "stt", Files: []string{"files/clip.ogg"}},
	} {
		if _, err := e.run(t, "alice", req); err != nil {
			t.Fatalf("%s: %v", req.Kind, err)
		}
	}
	for kind, n := range map[string]int{"text": 1, "image": 2, "speech": 1, "transcription": 1} {
		got := recs.byKind(kind)
		if len(got) < n {
			t.Errorf("%s: %d accounting records, want %d", kind, len(got), n)
		}
		for _, r := range got {
			if r["agent"] != want.Agent || r["agent_name"] != want.Name || r["project"] != want.Project {
				t.Errorf("%s record billed to %v/%v/%v, want %+v", kind, r["agent"], r["agent_name"], r["project"], want)
			}
		}
	}
}
