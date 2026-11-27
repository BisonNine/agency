// Agency
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/forum"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/state"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// scriptedChat is an OpenAI-compatible chat server whose reply depends on
// the request's last message: after a tool result it answers "done"; a
// message naming a step of steps calls that tool; anything else is "ok".
type scriptedChat struct {
	steps map[string]scriptedStep
}

// scriptedStep is a tool call the server answers with.
type scriptedStep struct {
	tool, args string
}

func (s *scriptedChat) handle(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Messages) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	last := req.Messages[len(req.Messages)-1]
	msg := map[string]any{"role": "assistant", "content": "ok"}
	finish := "stop"
	text, isText := last.Content.(string)
	if !isText {
		text = ""
	}
	if last.Role == "tool" {
		msg["content"] = "done"
	} else {
		for marker, call := range s.steps {
			if strings.Contains(text, marker) {
				msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
					"id": "call-1", "type": "function",
					"function": map[string]any{"name": call.tool, "arguments": call.args},
				}}}
				finish = "tool_calls"
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
	}); err != nil {
		return
	}
}

// originRig is a running loop of Alice, Bob and Carol on a scripted model
// whose calls are written as accounting records, with a forum host bound.
type originRig struct {
	al   *AgentLoop
	host *ForumHost
	recs *accountingRecords
}

// originSteps are the scripted model's steps: Alice spawns a clone of
// herself, a Bob worker, or a Bob worker that spawns Carol; Bob changes his
// billing code.
var originSteps = map[string]scriptedStep{
	"go-self":  {"agent_spawn", `{"task":"work-self","mode":"wait"}`},
	"go-bob":   {"agent_spawn", `{"agent_id":"bob","task":"work-bob","mode":"wait"}`},
	"go-chain": {"agent_spawn", `{"agent_id":"bob","task":"go-carol","mode":"wait"}`},
	"go-carol": {"agent_spawn", `{"agent_id":"carol","task":"work-carol","mode":"wait"}`},
	"go-bill":  {"billing", `{"code":"acme"}`},
}

func newOriginRig(t *testing.T) *originRig {
	t.Helper()
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tools.RegisterProvider(tools.NamespacedProvider("agent", toolsagents.GlobalProvider)) // idempotent
	recs := &accountingRecords{}
	accounting.SetLogger(slog.New(recs))
	t.Cleanup(func() { accounting.SetLogger(nil) })
	srv := httptest.NewServer(http.HandlerFunc((&scriptedChat{steps: originSteps}).handle))
	t.Cleanup(srv.Close)

	cfg := ownerTestConfig(t)
	cfg.Providers = []modelspec.Provider{{Name: "p", Protocol: "openai-chat", BaseURL: srv.URL + "/v1", APIKey: "k"}}
	cfg.Models = []modelspec.Model{{ModelName: "m", Model: "m-wire", Provider: "p", Enabled: true}}
	cfg.Agents.Defaults.Models = []string{"m"}
	cfg.Agents.List = []config.AgentConfig{
		{ID: "alice", Name: "Alice", Default: true, Tools: []string{"agent_spawn"}, Subagents: &config.SubagentsConfig{AllowAgents: []string{"bob"}}},
		{ID: "bob", Name: "Bob", Tools: []string{"agent_spawn"}, Subagents: &config.SubagentsConfig{AllowAgents: []string{"carol"}}},
		{ID: "carol", Name: "Carol"},
	}
	msgBus := bus.NewMessageBus()
	al := mustNewAgentLoop(t, cfg, msgBus, &mockProvider{}, realClients{}, OwnsDataDir())
	host := NewForumHost()
	host.Bind(al)
	ctx, cancel := context.WithCancel(context.Background())
	al.running.Store(true)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			msg, ok := msgBus.ConsumeInbound(ctx)
			if !ok {
				return
			}
			al.dispatchInbound(ctx, msg)
		}
	})
	t.Cleanup(func() {
		al.running.Store(false)
		cancel()
		wg.Wait()
		al.activeRequests.Wait()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		al.Close(closeCtx)
	})
	return &originRig{al: al, host: host, recs: recs}
}

// take returns the records written since the last take and forgets them.
func (r *originRig) take() []map[string]any {
	r.recs.mu.Lock()
	defer r.recs.mu.Unlock()
	out := r.recs.recs
	r.recs.recs = nil
	return out
}

// turn runs a turn of agentID's main session with message on ctx.
func (r *originRig) turn(t *testing.T, ctx context.Context, agentID, message string) {
	t.Helper()
	agent, ok := r.al.GetRegistry().Get(agentID)
	if !ok {
		t.Fatalf("no agent %s", agentID)
	}
	key := "agent:" + agentID + ":main"
	if _, err := r.al.runAgentLoop(ctx, agent, processOptions{SessionKey: key, Channel: "cli", ChatID: "direct", UserMessage: message}); err != nil {
		t.Fatalf("%s's turn: %v", agentID, err)
	}
}

// wantRecord is the accounting identity a record must show; agent is the
// exact agent_id, or a prefix ending in ':' for a temporary agent.
type wantRecord struct {
	origin, agent, name, project string
}

func (w wantRecord) matches(r map[string]any) bool {
	id, ok := r["agent_id"].(string)
	if !ok {
		return false
	}
	if strings.HasSuffix(w.agent, ":") {
		return strings.HasPrefix(id, w.agent) && len(id) == len(w.agent)+8
	}
	return id == w.agent
}

// checkRecords fails unless every record is one of want, with its origin,
// name and project, and each of want has at least one record.
func checkRecords(t *testing.T, recs []map[string]any, want ...wantRecord) {
	t.Helper()
	seen := make([]bool, len(want))
	for _, r := range recs {
		found := false
		for i, w := range want {
			if !w.matches(r) {
				continue
			}
			found, seen[i] = true, true
			if r["origin_agent"] != w.origin || r["agent_name"] != w.name || r["project"] != w.project {
				t.Errorf("record %v/%v/%v/%v, want %+v", r["origin_agent"], r["agent_id"], r["agent_name"], r["project"], w)
			}
		}
		if !found {
			t.Errorf("unexpected record %v/%v/%v/%v", r["origin_agent"], r["agent_id"], r["agent_name"], r["project"])
		}
	}
	for i, w := range want {
		if !seen[i] {
			t.Errorf("no record for %+v", w)
		}
	}
}

// Each row of the accounting identity table: who caused the call
// (origin_agent), who made it (agent_id, agent_name), and its billing code,
// as written to the accounting log.
func TestAccountingRecords_OriginAgentName(t *testing.T) {
	r := newOriginRig(t)
	ctx := context.Background()
	alice, _ := r.al.GetRegistry().Get("alice")
	if err := r.al.setSessionBilling(alice, "agent:alice:main", "garden"); err != nil {
		t.Fatal(err)
	}
	own := wantRecord{"alice", "alice", "Alice", "garden"}

	t.Run("Alice's own turn", func(t *testing.T) {
		r.turn(t, ctx, "alice", "hello")
		checkRecords(t, r.take(), own)
	})
	t.Run("Alice spawns a clone of itself", func(t *testing.T) {
		r.turn(t, ctx, "alice", "go-self")
		checkRecords(t, r.take(), own, wantRecord{"alice", "alice:", "Alice", "garden"})
	})
	t.Run("Alice spawns a Bob worker", func(t *testing.T) {
		r.turn(t, ctx, "alice", "go-bob")
		checkRecords(t, r.take(), own, wantRecord{"alice", "bob:", "Bob", "garden"})
	})
	t.Run("that Bob worker spawns Carol", func(t *testing.T) {
		r.turn(t, ctx, "alice", "go-chain")
		checkRecords(t, r.take(), own,
			wantRecord{"alice", "bob:", "Bob", "garden"}, wantRecord{"alice", "carol:", "Carol", "garden"})
	})

	ask := func(t *testing.T, origin forum.Origin, participant string) {
		t.Helper()
		askCtx := forum.WithAskInfo(ctx, forum.AskInfo{ForumID: "f1", Origin: origin})
		if _, err := r.host.Ask(askCtx, participant, "hello", 10*time.Second); err != nil {
			t.Fatalf("Ask: %v", err)
		}
	}
	t.Run("Bob in Alice's forum, as himself", func(t *testing.T) {
		ask(t, forum.Origin{AgentID: "alice", Billing: "garden", OriginAgent: "alice"}, "bob")
		checkRecords(t, r.take(), wantRecord{"alice", "bob", "Bob", "garden"})
	})
	t.Run("model-only participant in Alice's forum", func(t *testing.T) {
		id, err := r.host.CreateFresh(ctx, forum.FreshSpec{Owner: "alice", Model: "m", Mode: forum.FreshModeNoMemory})
		if err != nil {
			t.Fatalf("CreateFresh: %v", err)
		}
		ask(t, forum.Origin{AgentID: "alice", Billing: "garden", OriginAgent: "alice"}, id)
		checkRecords(t, r.take(), wantRecord{"alice", "temp:", "temp", "garden"})
	})
	t.Run("forum run launched before origins", func(t *testing.T) {
		ask(t, forum.Origin{AgentID: "alice", Billing: "garden"}, "bob")
		checkRecords(t, r.take(), wantRecord{"bob", "bob", "Bob", "garden"})
	})

	// A scheduled job's run is a message carrying the job's code and
	// origin (tools/schedule pins that the job keeps them).
	job := func(t *testing.T, agentID, origin string) {
		t.Helper()
		msg := toAgent(agentID, "job-"+agentID+origin, "time to stretch")
		msg.Metadata = bus.SetOrigin(bus.SetBilling(msg.Metadata, "garden"), origin)
		if _, err := r.al.processMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("scheduled job run as Alice", func(t *testing.T) {
		job(t, "alice", "alice")
		checkRecords(t, r.take(), own)
	})
	t.Run("job Alice scheduled to run as Bob", func(t *testing.T) {
		job(t, "bob", "alice")
		checkRecords(t, r.take(), wantRecord{"alice", "bob", "Bob", "garden"})
	})
	t.Run("job added from the command line, run as Bob", func(t *testing.T) {
		job(t, "bob", "")
		checkRecords(t, r.take(), wantRecord{"bob", "bob", "Bob", "garden"})
	})
}

// identityModel is forumModel recording the accounting identity of every
// call, which is what the call's accounting record shows.
type identityModel struct {
	*forumModel
	mu  sync.Mutex
	ids []accounting.Identity
}

func (m *identityModel) Chat(ctx context.Context, id accounting.Identity, messages []spawnllm.Message,
	defs []spawnllm.ToolDefinition, model string, opts map[string]any,
) (*spawnllm.LLMResponse, error) {
	m.mu.Lock()
	m.ids = append(m.ids, id)
	m.mu.Unlock()
	return m.forumModel.Chat(ctx, id, messages, defs, model, opts)
}

// A forum launched with no turn above it (a tool client connected as Alice,
// whose calls run on Alice's tool registry) has Alice as its origin, and
// Bob's participant turn has origin_agent alice, agent_id bob.
func TestForumLaunch_OriginIsTheLauncher(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	model := &identityModel{forumModel: &forumModel{}}
	r := newForumRig(t, forumConfig(t, true, true), model)

	id := launchForum(t, r.al, "telegram")
	waitCompleted(t, r, id)

	alice, _ := r.al.GetRegistry().Get("alice")
	raw, err := os.ReadFile(filepath.Join(alice.Workspace, "forums", id, "runs", "1", "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap forum.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Origin.AgentID != "alice" || snap.Origin.OriginAgent != "alice" {
		t.Errorf("the run's origin = %+v, want launcher and origin alice", snap.Origin)
	}
	model.mu.Lock()
	ids := slices.Clone(model.ids)
	model.mu.Unlock()
	var bob int
	for _, got := range ids {
		if got.Agent != "bob" {
			continue
		}
		bob++
		if got.Origin != "alice" {
			t.Errorf("Bob's forum call has origin_agent %q, want alice", got.Origin)
		}
	}
	if bob == 0 {
		t.Errorf("no call of Bob's forum turn in %+v", ids)
	}
}

// The billing tool changes the code of the work, never its origin.
func TestBillingTool_KeepsTheOrigin(t *testing.T) {
	r := newOriginRig(t)
	bob, _ := r.al.GetRegistry().Get("bob")
	withBillingTool(t, r.al, bob)
	ctx := tools.WithOrigin(tools.WithBilling(context.Background(), "garden"), "alice")
	r.turn(t, ctx, "bob", "go-bill")
	recs := r.take()
	if len(recs) != 2 {
		t.Fatalf("%d records, want the call before the change and the one after", len(recs))
	}
	checkRecords(t, recs[:1], wantRecord{"alice", "bob", "Bob", "garden"})
	checkRecords(t, recs[1:], wantRecord{"alice", "bob", "Bob", "acme"})
	if got := tools.Origin(ctx); got != "alice" {
		t.Errorf("the work's origin = %q, want alice", got)
	}
}

// originRecorder answers every call and records the origin of each.
type originRecorder struct {
	mu      sync.Mutex
	origins []string
}

func (o *originRecorder) Chat(_ context.Context, id accounting.Identity, _ []spawnllm.Message,
	_ []spawnllm.ToolDefinition, _ string, _ map[string]any,
) (*spawnllm.LLMResponse, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.origins = append(o.origins, id.Origin)
	return &spawnllm.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (o *originRecorder) GetDefaultModel() string { return "test-model" }

func (o *originRecorder) last(t *testing.T) string {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.origins) == 0 {
		t.Fatal("no model call was made")
	}
	return o.origins[len(o.origins)-1]
}

// Summaries and vision describe calls have the origin of the work they run
// for, else the agent's own; memory consolidation always the agent's own.
func TestSummaryVisionAndMemoryOrigin(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	rec := &originRecorder{}
	setTestProvider(main, rec)
	inherited := tools.WithOrigin(context.Background(), "alice")

	caller, _ := al.newCompressModelCaller(main, mainSession, models.Summarize)
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{{context.Background(), "main"}, {inherited, "alice"}} {
		if _, _, _, err := caller.complete(tc.ctx, "sys", "user", false, nil); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t); got != tc.want {
			t.Errorf("summary has origin %q, want %q", got, tc.want)
		}
	}
	if _, err := al.NewMemoryModelCaller(main).Complete(inherited, hook.CompleteRequest{System: "s", User: "u"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got != "main" {
		t.Errorf("memory consolidation has origin %q, want main", got)
	}
	vc, ok := al.buildVisionLLMClient(al.GetConfig(), main, "test-model")
	if !ok {
		t.Fatal("no vision client")
	}
	if _, err := vc.Complete(inherited, []spawnllm.Message{{Role: "user", Content: "look"}}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got != "alice" {
		t.Errorf("vision describe has origin %q, want alice", got)
	}
}

// An inbound message's origin is the turn's; messages with different
// origins are never merged into one turn.
func TestInboundOrigin(t *testing.T) {
	ctx := context.Background()
	if got := tools.Origin(withInboundOrigin(ctx, bus.InboundMessage{})); got != "" {
		t.Errorf("a message without an origin gave %q", got)
	}
	msg := bus.InboundMessage{Metadata: map[string]string{bus.MetaOrigin: "alice"}}
	if got := tools.Origin(withInboundOrigin(ctx, msg)); got != "alice" {
		t.Errorf("carried origin = %q, want alice", got)
	}

	queued := func(id, origin string) bus.InboundMessage {
		return bus.InboundMessage{
			Channel: "telegram", ChatID: "c", MessageID: id, Content: id,
			Metadata: bus.SetOrigin(map[string]string{}, origin),
		}
	}
	pending := []bus.InboundMessage{queued("1", ""), queued("2", ""), queued("3", "alice"), queued("4", "alice")}
	first, _ := takeBatch(&pending, false)
	if first.Content != "1\n2" || len(pending) != 2 {
		t.Fatalf("first batch %q, %d left", first.Content, len(pending))
	}
	if second, _ := takeBatch(&pending, false); second.Content != "3\n4" || second.Metadata[bus.MetaOrigin] != "alice" {
		t.Fatalf("second batch %q has origin %q", second.Content, second.Metadata[bus.MetaOrigin])
	}
}

// An ask carries the asking work's origin; a background result the turn's
// only when the turn inherited it; a relaunched task's result the one it
// recorded.
func TestOriginCarriedOnMessages(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	scoped := hook.WithScope(context.Background(), hook.Scope{AgentID: "main", Session: mainSession})
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{
		{scoped, "main"},
		{hook.WithScope(context.Background(), hook.Scope{AgentID: "main", Session: mainSession, Origin: "bob"}), "bob"},
		{tools.WithOrigin(scoped, "alice"), "alice"},
	} {
		if err := al.publishAsk(tc.ctx, "ask-"+tc.want, sender{name: "Main"}, main, "hi", nil, 0); err != nil {
			t.Fatal(err)
		}
		if in, ok := consumeInbound(t, tl.msgBus); !ok || in.Metadata[bus.MetaOrigin] != tc.want {
			t.Errorf("ask carries origin %q, want %q", in.Metadata[bus.MetaOrigin], tc.want)
		}
	}

	opts := processOptions{Channel: "telegram", ChatID: "c1", SessionKey: mainSession}
	al.asyncToolCallback(scoped, main, opts, "slow")(scoped, &toolkit.Result{ForLLM: "result"})
	if in, ok := consumeInbound(t, tl.msgBus); !ok {
		t.Fatal("no background result")
	} else if _, has := in.Metadata[bus.MetaOrigin]; has {
		t.Errorf("result of a turn with its own origin carries %q", in.Metadata[bus.MetaOrigin])
	}
	inherited := tools.WithOrigin(scoped, "alice")
	al.asyncToolCallback(inherited, main, opts, "slow")(inherited, &toolkit.Result{ForLLM: "result"})
	if in, _ := consumeInbound(t, tl.msgBus); in.Metadata[bus.MetaOrigin] != "alice" {
		t.Errorf("result of an inherited turn carries %q, want alice", in.Metadata[bus.MetaOrigin])
	}

	al.taskPointerCallback("telegram", "c1", "main", 0, "", "alice")(context.Background(), &toolkit.Result{ForLLM: "done"})
	if in, _ := consumeInbound(t, tl.msgBus); in.Metadata[bus.MetaOrigin] != "alice" {
		t.Errorf("relaunched task's result carries %q, want alice", in.Metadata[bus.MetaOrigin])
	}
}

// A turn's inherited origin is recorded with its pending turn and replayed
// after a restart; a turn with its own origin records none.
func TestPendingTurn_Origin(t *testing.T) {
	tl, store := newRecoveryTestLoop(t)
	main := mustGetAgent(t, tl.al)
	opts := processOptions{SessionKey: testSessionKey, Channel: "webui", ChatID: "chat-7", MessageID: "m1"}
	tl.al.recordPendingTurnSource(tools.WithOrigin(context.Background(), "alice"), main, opts)
	sm := testStateManager(t, tl.al, "main")
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Origin != "alice" {
		t.Fatalf("pending turn has origin %q, want alice", pt.Origin)
	}

	if err := sm.SetPendingTurn(testSessionKey, state.PendingTurn{Channel: "webui", ChatID: "chat-7", Origin: "alice"}); err != nil {
		t.Fatal(err)
	}
	tl.al.recoverSession(context.Background(), "main", testSessionKey, store)
	consumeOutbound(t, tl.msgBus) // the replay notice
	if msg, ok := consumeInbound(t, tl.msgBus); !ok || msg.Metadata[bus.MetaOrigin] != "alice" {
		t.Fatalf("replay carries origin %q, want alice", msg.Metadata[bus.MetaOrigin])
	}

	tl.al.recordPendingTurnSource(context.Background(), main, opts)
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Origin != "" {
		t.Errorf("a turn with its own origin recorded %q", pt.Origin)
	}
}

// A classifier's tool runs with the classifier's agent as its origin, never
// one inherited from where it was called.
func TestClassifierHost_CallOrigin(t *testing.T) {
	al, host, _ := newClassifierHostLoop(t, classifierHostConfig(t, "http://127.0.0.1:0"))
	var seen string
	registerOn(t, al, "alice", originProbe{origin: &seen})
	if _, err := host.Call(tools.WithOrigin(context.Background(), "bob"), "Alice", "inbox_list", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if seen != "alice" {
		t.Errorf("classifier tool has origin %q, want alice", seen)
	}
}

// originProbe records the origin of its call.
type originProbe struct{ origin *string }

func (o originProbe) Name() string               { return "inbox_list" }
func (o originProbe) Description() string        { return "records its origin" }
func (o originProbe) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (o originProbe) Execute(ctx context.Context, _ map[string]any) *toolkit.Result {
	*o.origin = tools.Origin(ctx)
	return toolkit.NewResult("[]")
}
