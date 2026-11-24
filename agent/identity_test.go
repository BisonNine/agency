// Agency
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// identityRecorder is a provider that answers every call and records the
// identity each call was billed to.
type identityRecorder struct {
	mu  sync.Mutex
	ids []accounting.Identity
}

func (r *identityRecorder) Chat(_ context.Context, id accounting.Identity, _ []spawnllm.Message,
	_ []spawnllm.ToolDefinition, _ string, _ map[string]any,
) (*spawnllm.LLMResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return &spawnllm.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (r *identityRecorder) GetDefaultModel() string { return "test-model" }

func (r *identityRecorder) seen() []accounting.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]accounting.Identity(nil), r.ids...)
}

// TestAgentInstance_Identity: a config agent is billed to itself under its
// display name, a clone to its source under its own label, and a fresh
// temporary agent to its own id under the name it was given. Identity names
// no billing code: every caller sets the one its call is billed to.
func TestAgentInstance_Identity(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	reg := newTestAgentLoop(t).al.GetRegistry()
	main, _ := reg.Get("main")
	if got, want := main.Identity(), (accounting.Identity{Agent: "main", Name: "Main"}); got != want {
		t.Errorf("config agent identity = %+v, want %+v", got, want)
	}

	cloneID, err := reg.CreateClone("main", agentreg.EphemeralMemory())
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)
	want := accounting.Identity{Agent: "main", Name: "main (clone " + agentreg.ShortID(cloneID) + ")"}
	if got := clone.Identity(); got != want {
		t.Errorf("clone identity = %+v, want %+v", got, want)
	}

	freshID, err := reg.CreateFresh(config.AgentConfig{Name: "Bob"}, agentreg.WithSystemPrompt("You are Bob."))
	if err != nil {
		t.Fatalf("CreateFresh: %v", err)
	}
	fresh, _ := reg.Get(freshID)
	if got, want := fresh.Identity(), (accounting.Identity{Agent: freshID, Name: "Bob"}); got != want {
		t.Errorf("fresh agent identity = %+v, want %+v", got, want)
	}
}

// TestIdentityOf: a running agent is billed by its Identity, an id the
// registry does not hold by the id itself, and an empty id to no agent, each
// under the billing code given.
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
		want accounting.Identity
	}{
		{"config agent", "Main", billed(main.Identity(), "acme")},
		{"clone", cloneID, billed(clone.Identity(), "acme")},
		{"unknown", "Alice", accounting.Identity{Agent: "alice", Name: "Alice", Project: "acme"}},
		{"empty", " ", global.NoOwnerIdentity("acme")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := al.identityOf(tt.id, "acme"); got != tt.want {
				t.Errorf("identityOf(%q) = %+v, want %+v", tt.id, got, tt.want)
			}
		})
	}
}

// TestTurn_BillsTheAgent: every model call of a turn carries the turn's agent
// identity under the default billing code, named; a clone's turn is billed to
// its source.
func TestTurn_BillsTheAgent(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	al := newTestAgentLoop(t).al
	reg := al.GetRegistry()
	main, _ := reg.Get("main")
	rec := &identityRecorder{}
	setTestProvider(main, rec)
	if _, err := al.ProcessDirect(context.Background(), "hello", "direct-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}

	cloneID, err := reg.CreateClone("main", agentreg.EphemeralMemory())
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)
	cloneRec := &identityRecorder{}
	setTestProvider(clone, cloneRec)
	key := routing.BuildAgentMainSessionKey(clone.ID)
	if _, err := al.runAgentLoop(context.Background(), clone, processOptions{
		SessionKey: key, Channel: "subagent", ChatID: key, UserMessage: "hello from the clone",
	}); err != nil {
		t.Fatalf("clone turn: %v", err)
	}

	for _, tc := range []struct {
		name string
		rec  *identityRecorder
		want accounting.Identity
	}{
		{"agent", rec, billed(main.Identity(), global.DefaultBilling)},
		{"clone", cloneRec, billed(clone.Identity(), global.DefaultBilling)},
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
// conversation they summarize, under the code given.
func TestCompressModelCaller_BillsTheAgent(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	al := newTestAgentLoop(t).al
	main, _ := al.GetRegistry().Get("main")
	caller, _ := al.newCompressModelCaller(main, "agent:main:main", models.Summarize)
	if caller.id != main.Identity() {
		t.Fatalf("compression caller identity = %+v, want %+v", caller.id, main.Identity())
	}

	rec := &identityRecorder{}
	cat := compressCatalog(map[string]spawnllm.LLMProvider{"m": rec})
	want := billed(main.Identity(), "acme")
	c := withID(compressCaller(cat, "m"), want)
	if _, _, _, err := c.complete(context.Background(), "sys", "user", false, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if ids := rec.seen(); len(ids) != 1 || ids[0] != want {
		t.Fatalf("summary calls billed to %+v, want [%+v]", ids, want)
	}
}

// billed is id billed to code.
func billed(id accounting.Identity, code string) accounting.Identity {
	id.Project = code
	return id
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
	want := accounting.Identity{Agent: "alice", Name: "alice (clone 1a2b3c4d)", Project: "default"}
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

// codeRecorder answers every call and records the billing code each was
// billed to; an empty code fails the test, since outside a turn the SDK
// would silently bill it to the default.
type codeRecorder struct {
	t     *testing.T
	mu    sync.Mutex
	codes []string
}

func (r *codeRecorder) Chat(_ context.Context, id accounting.Identity, _ []spawnllm.Message,
	_ []spawnllm.ToolDefinition, _ string, _ map[string]any,
) (*spawnllm.LLMResponse, error) {
	if id.Project == "" {
		r.t.Errorf("a call for %s named no billing code", id.Agent)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, id.Project)
	return &spawnllm.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (r *codeRecorder) GetDefaultModel() string { return "test-model" }

// last is the code of the latest call; it fails the test when there was
// no call since mark.
func (r *codeRecorder) last(t *testing.T, mark int) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.codes) <= mark {
		t.Fatal("no model call was made")
	}
	return r.codes[len(r.codes)-1]
}

func (r *codeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.codes)
}

// TestBilling_EveryCallSiteNamesACode walks the call sites of the billing
// plan's table (3.2) that run in the agent loop: each model call is billed
// to the code its site names, never to an empty one. The sites outside the
// loop are pinned where they live: the task records and agent_spawn
// (tools/agents), Maestro (tools/maestro), MCP calls and their background
// results (mcpserver), scheduled jobs (tools/schedule), the classifier
// (TestClassifierHost_CompleteBilling), forum participants
// (TestForumHost_AskBilling), voice notes (TestPrepareInbound_VoiceBillingCode),
// `claw cron add` (internal/cron) and `claw test` (internal/test).
func TestBilling_EveryCallSiteNamesACode(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()
	tl := newTestAgentLoop(t)
	al := tl.al
	main, _ := al.GetRegistry().Get("main")
	rec := &codeRecorder{t: t}
	setTestProvider(main, rec)
	if err := al.setSessionBilling(main, "agent:main:main", "garden"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("main turn", func(t *testing.T) {
		mark := rec.count()
		if _, err := al.runAgentLoop(ctx, main, processOptions{SessionKey: "agent:main:main", Channel: "cli", ChatID: "direct", UserMessage: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "garden" {
			t.Errorf("billed %q, want the session's garden", got)
		}
	})
	t.Run("a new session's turn", func(t *testing.T) {
		mark := rec.count()
		if _, err := al.runAgentLoop(ctx, main, processOptions{SessionKey: "agent:main:other", Channel: "cli", ChatID: "direct", UserMessage: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != global.DefaultBilling {
			t.Errorf("billed %q, want the default", got)
		}
	})
	t.Run("message carrying a code (ask, scheduled job, replay, background result)", func(t *testing.T) {
		mark := rec.count()
		msg := toAgent("main", "m1", "from a job")
		msg.Metadata[bus.MetaBilling] = "acme"
		if _, err := al.processMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "acme" {
			t.Errorf("billed %q, want acme", got)
		}
	})
	t.Run("sub-agent or clone", func(t *testing.T) {
		cloneID, err := al.GetRegistry().CreateClone("main", agentreg.EphemeralMemory())
		if err != nil {
			t.Fatal(err)
		}
		clone, _ := al.GetRegistry().Get(cloneID)
		setTestProvider(clone, rec)
		key := routing.BuildAgentMainSessionKey(clone.ID)
		mark := rec.count()
		if _, err := al.runAgentLoop(tools.WithBilling(ctx, "acme"), clone, processOptions{SessionKey: key, Channel: "subagent", ChatID: key, UserMessage: "work"}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "acme" {
			t.Errorf("billed %q, want the task's acme", got)
		}
	})
	t.Run("summary", func(t *testing.T) {
		caller, _ := al.newCompressModelCaller(main, "agent:main:main", models.Summarize)
		mark := rec.count()
		if _, _, _, err := caller.complete(ctx, "sys", "user", false, nil); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "garden" {
			t.Errorf("billed %q, want the session's garden", got)
		}
		mark = rec.count()
		if _, _, _, err := caller.complete(tools.WithBilling(ctx, "acme"), "sys", "user", false, nil); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "acme" {
			t.Errorf("summary in a turn billed %q, want the turn's acme", got)
		}
	})
	t.Run("memory consolidation", func(t *testing.T) {
		mark := rec.count()
		if _, err := al.NewMemoryModelCaller(main).Complete(ctx, hook.CompleteRequest{System: "s", User: "u"}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != global.MemoryBilling {
			t.Errorf("billed %q, want memory", got)
		}
	})
	t.Run("vision describe", func(t *testing.T) {
		vc, ok := al.buildVisionLLMClient(al.GetConfig(), main, "test-model")
		if !ok {
			t.Fatal("no vision client")
		}
		mark := rec.count()
		if _, err := vc.Complete(tools.WithBilling(ctx, "acme"), []spawnllm.Message{{Role: "user", Content: "look"}}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != "acme" {
			t.Errorf("billed %q, want acme", got)
		}
		mark = rec.count()
		if _, err := vc.Complete(ctx, []spawnllm.Message{{Role: "user", Content: "look"}}); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t, mark); got != global.DefaultBilling {
			t.Errorf("billed %q outside a session, want the default", got)
		}
	})
}
