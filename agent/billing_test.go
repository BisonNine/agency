// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/classifier"
	"go.bison9.com/agency/forum"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/state"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
	toolsbilling "go.bison9.com/agency/tools/billing"
)

const mainSession = "agent:main:main"

// billingLoop is a test loop and its default agent, main.
func billingLoop(t *testing.T) (*testLoop, *AgentInstance) {
	t.Helper()
	restore := logger.RedirectForTest(&safeBufLoop{})
	t.Cleanup(restore)
	tl := newTestAgentLoop(t)
	return tl, mustGetAgent(t, tl.al)
}

// stateStore is agent's session store as a compaction state store.
func stateStore(t *testing.T, agent *AgentInstance) compactionStateStore {
	t.Helper()
	store, ok := agent.Sessions.(compactionStateStore)
	if !ok {
		t.Fatalf("session store %T keeps no state", agent.Sessions)
	}
	return store
}

// storedBilling is the session's billing code as its state row holds it.
func storedBilling(t *testing.T, agent *AgentInstance, sessionKey string) string {
	t.Helper()
	st, err := stateStore(t, agent).GetCompactionState(sessionKey)
	if err != nil {
		t.Fatalf("GetCompactionState: %v", err)
	}
	return st.Project
}

// TestBillingFor: an inherited code wins, then the session's stored code,
// then the default; the session is the one named, else the call's.
func TestBillingFor(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	ctx := context.Background()
	if got := al.billingFor(ctx, main, mainSession); got != global.DefaultBilling {
		t.Fatalf("new session = %q, want the default", got)
	}
	if err := al.setSessionBilling(main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	if got := al.billingFor(ctx, main, mainSession); got != "garden" {
		t.Errorf("session code = %q, want garden", got)
	}
	if got := al.billingFor(ctx, main, "agent:main:other"); got != global.DefaultBilling {
		t.Errorf("another session = %q, want the default", got)
	}
	if got := al.billingFor(tools.WithBilling(ctx, "acme"), main, mainSession); got != "acme" {
		t.Errorf("inherited code = %q, want acme", got)
	}
	// The session is the call's when none is named: its scope's, else its
	// tool context's.
	scoped := hook.WithScope(ctx, hook.Scope{AgentID: "main", Session: mainSession})
	if got := al.billingFor(scoped, main, ""); got != "garden" {
		t.Errorf("scope's session = %q, want garden", got)
	}
	if got := al.billingFor(toolkit.WithSessionKey(ctx, mainSession), main, ""); got != "garden" {
		t.Errorf("tool context's session = %q, want garden", got)
	}
	if got := al.billingFor(ctx, nil, mainSession); got != global.DefaultBilling {
		t.Errorf("no agent = %q, want the default", got)
	}
	if got := al.CallerBilling(scoped); got != "garden" {
		t.Errorf("CallerBilling with a scope = %q, want garden", got)
	}
	if got := al.CallerBilling(tools.WithBilling(scoped, "acme")); got != "acme" {
		t.Errorf("CallerBilling inherited = %q, want acme", got)
	}
	if got := al.CallerBilling(hook.WithScope(ctx, hook.Scope{AgentID: "nobody", Session: "x"})); got != global.DefaultBilling {
		t.Errorf("CallerBilling of an unknown agent = %q, want the default", got)
	}
	if got := al.CallerBilling(ctx); got != global.DefaultBilling {
		t.Errorf("CallerBilling without a scope = %q, want the default", got)
	}
	id := al.identityFor(ctx, main, mainSession)
	if id != billed(main.Identity(), "garden") {
		t.Errorf("identityFor = %+v", id)
	}
}

// A stored code that is not one (a hand-edited row) is ignored.
func TestBillingFor_InvalidStoredCode(t *testing.T) {
	tl, main := billingLoop(t)
	store := stateStore(t, main)
	st, err := store.GetCompactionState(mainSession)
	if err != nil {
		t.Fatal(err)
	}
	st.Project = "not a code"
	if err := store.SetCompactionState(mainSession, st); err != nil {
		t.Fatal(err)
	}
	if got := tl.al.billingFor(context.Background(), main, mainSession); got != global.DefaultBilling {
		t.Errorf("invalid stored code gives %q, want the default", got)
	}
}

// TestSetBilling: a session's code is stored in its state row; inherited
// work changes only its own code; an invalid code is refused.
func TestSetBilling(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	ctx := context.Background()
	if err := al.setBilling(ctx, main, mainSession, "bad code"); !errors.Is(err, accounting.ErrInvalidProject) {
		t.Fatalf("invalid code: %v", err)
	}
	if err := al.setBilling(ctx, main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	if got := storedBilling(t, main, mainSession); got != "garden" {
		t.Errorf("stored code = %q, want garden", got)
	}
	// The session of the call when none is named.
	scoped := hook.WithScope(ctx, hook.Scope{AgentID: "main", Session: "agent:main:other"})
	if err := al.setBilling(scoped, main, "", "orchard"); err != nil {
		t.Fatal(err)
	}
	if got := al.billingFor(ctx, main, "agent:main:other"); got != "orchard" {
		t.Errorf("the call's session = %q, want orchard", got)
	}
	if err := al.setBilling(ctx, main, "", "orchard"); !errors.Is(err, errNoBillingSession) {
		t.Errorf("no session: %v, want errNoBillingSession", err)
	}

	inherited := tools.WithBilling(ctx, "acme")
	var told []string
	tools.InheritedBillingOf(inherited).OnSet(func(code string) { told = append(told, code) })
	if err := al.setBilling(inherited, main, mainSession, "acme-2"); err != nil {
		t.Fatal(err)
	}
	if got := al.billingFor(inherited, main, mainSession); got != "acme-2" {
		t.Errorf("inherited work's code = %q, want acme-2", got)
	}
	if len(told) != 1 || told[0] != "acme-2" {
		t.Errorf("the run's records were told %v", told)
	}
	if got := al.billingFor(ctx, main, mainSession); got != "garden" {
		t.Errorf("the session's code = %q after inherited work changed its own, want garden", got)
	}
}

// brokenStateStore is a session store whose state cannot be read or
// written.
type brokenStateStore struct {
	recoveryTestStore
	getErr, setErr error
}

func (s *brokenStateStore) GetCompactionState(string) (memory.CompactionState, error) {
	return memory.CompactionState{}, s.getErr
}

func (s *brokenStateStore) SetCompactionState(string, memory.CompactionState) error {
	return s.setErr
}

// A session whose state cannot be read or written keeps no code: the set
// fails and the default stays.
func TestSetBilling_StoreFailures(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	orig := main.Sessions
	t.Cleanup(func() { main.Sessions = orig })

	main.Sessions = &brokenStateStore{getErr: errors.New("unreadable")}
	if err := al.setSessionBilling(main, "s1", "acme"); err == nil {
		t.Error("set on an unreadable state succeeded")
	}
	if got := al.billingFor(context.Background(), main, "s1"); got != global.DefaultBilling {
		t.Errorf("unreadable state gives %q, want the default", got)
	}
	main.Sessions = &brokenStateStore{setErr: errors.New("disk full")}
	if err := al.setSessionBilling(main, "s2", "acme"); err == nil {
		t.Error("set on an unwritable state succeeded")
	}
	main.Sessions = &recoveryTestStore{}
	if err := al.setSessionBilling(main, "s3", "acme"); !errors.Is(err, errNoBillingSession) {
		t.Errorf("store without state: %v", err)
	}
	if got := al.sessionBillingCode(main, "s3"); got != "" {
		t.Errorf("store without state has code %q", got)
	}
}

// A session's code survives a restart and a clear of its conversation.
func TestSessionBilling_SurvivesRestartAndClear(t *testing.T) {
	tl, main := billingLoop(t)
	if err := tl.al.setSessionBilling(main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	if err := main.Sessions.TruncateHistory(mainSession, 0); err != nil {
		t.Fatal(err)
	}
	tl.al.forgetSessionCaches(main, mainSession)
	if got := tl.al.billingFor(context.Background(), main, mainSession); got != "garden" {
		t.Errorf("after a clear = %q, want garden", got)
	}

	tl.al.GetRegistry().Close()
	restarted := mustNewAgentLoop(t, tl.cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	again := mustGetAgent(t, restarted)
	if got := restarted.billingFor(context.Background(), again, mainSession); got != "garden" {
		t.Errorf("after a restart = %q, want garden", got)
	}
}

// billingRecorder answers from a script and records the billing code of
// each call.
type billingRecorder struct {
	mu        sync.Mutex
	responses []*spawnllm.LLMResponse
	codes     []string
}

func (r *billingRecorder) Chat(_ context.Context, id accounting.Identity, _ []spawnllm.Message,
	_ []spawnllm.ToolDefinition, _ string, _ map[string]any,
) (*spawnllm.LLMResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, id.Project)
	if len(r.responses) == 0 {
		return &spawnllm.LLMResponse{Content: "done", FinishReason: "stop"}, nil
	}
	resp := r.responses[0]
	r.responses = r.responses[1:]
	return resp, nil
}

func (r *billingRecorder) GetDefaultModel() string { return "test-model" }

func (r *billingRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.codes...)
}

// setBillingCall is a model reply calling the billing tool with code.
func setBillingCall(code string) *spawnllm.LLMResponse {
	return &spawnllm.LLMResponse{ToolCalls: []spawnllm.ToolCall{{
		ID: "b1", Type: "function", Name: "billing",
		Function: &spawnllm.FunctionCall{Name: "billing", Arguments: `{"code":"` + code + `"}`},
	}}}
}

// withBillingTool registers the billing tool on agent, bound to the loop's
// billing closures as registerAgentTools binds them.
func withBillingTool(t *testing.T, al *AgentLoop, agent *AgentInstance) {
	t.Helper()
	deps := tools.ToolDeps{
		Cfg: al.GetConfig(), AgentID: agent.ID,
		Billing: func(ctx context.Context) string { return al.billingFor(ctx, agent, "") },
		SetBilling: func(ctx context.Context, sessionKey, code string) error {
			return al.setBilling(ctx, agent, sessionKey, code)
		},
	}
	built := tools.BareNamespacedProvider(toolsbilling.Suite, toolsbilling.GlobalProvider).Build(deps)
	if len(built) != 1 {
		t.Fatalf("billing provider built %d tools", len(built))
	}
	agent.Tools.RegisterSuite(built[0])
}

// The billing tool changes the code from the next model call of the same
// turn, and it stays for the session's next turns.
func TestTurn_BillingToolChangesTheNextCall(t *testing.T) {
	tl, main := billingLoop(t)
	withBillingTool(t, tl.al, main)
	rec := &billingRecorder{responses: []*spawnllm.LLMResponse{setBillingCall("acme")}}
	setTestProvider(main, rec)
	if _, err := tl.al.runAgentLoop(context.Background(), main, processOptions{
		SessionKey: mainSession, Channel: "cli", ChatID: "direct", UserMessage: "bill acme",
	}); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); len(got) != 2 || got[0] != global.DefaultBilling || got[1] != "acme" {
		t.Fatalf("calls billed to %v, want [default acme]", got)
	}
	if _, err := tl.al.runAgentLoop(context.Background(), main, processOptions{
		SessionKey: mainSession, Channel: "cli", ChatID: "direct", UserMessage: "again",
	}); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); got[len(got)-1] != "acme" {
		t.Errorf("next turn billed to %q, want acme", got[len(got)-1])
	}
	if got := storedBilling(t, main, mainSession); got != "acme" {
		t.Errorf("stored code = %q, want acme", got)
	}
}

// A turn that inherited a code is billed to it; the billing tool changes it
// for that turn only, never the session's own code.
func TestTurn_InheritedBilling(t *testing.T) {
	tl, main := billingLoop(t)
	withBillingTool(t, tl.al, main)
	if err := tl.al.setSessionBilling(main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	rec := &billingRecorder{responses: []*spawnllm.LLMResponse{setBillingCall("acme-2")}}
	setTestProvider(main, rec)
	ctx := tools.WithBilling(context.Background(), "acme")
	if _, err := tl.al.runAgentLoop(ctx, main, processOptions{
		SessionKey: mainSession, Channel: "cli", ChatID: "direct", UserMessage: "scheduled work",
	}); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); len(got) != 2 || got[0] != "acme" || got[1] != "acme-2" {
		t.Fatalf("calls billed to %v, want [acme acme-2]", got)
	}
	if got := storedBilling(t, main, mainSession); got != "garden" {
		t.Errorf("session code = %q, want garden kept", got)
	}
	if code, _ := tools.Billing(ctx); code != "acme-2" {
		t.Errorf("the run's code = %q, want acme-2", code)
	}
}

func TestWithInboundBilling(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()
	ctx := context.Background()
	if _, ok := tools.Billing(withInboundBilling(ctx, bus.InboundMessage{})); ok {
		t.Error("a message without a code gave one")
	}
	msg := bus.InboundMessage{Metadata: map[string]string{bus.MetaBilling: "acme"}}
	if code, ok := tools.Billing(withInboundBilling(ctx, msg)); !ok || code != "acme" {
		t.Errorf("carried code = %q (%v), want acme", code, ok)
	}
	msg.Metadata[bus.MetaBilling] = "a b"
	if code, _ := tools.Billing(withInboundBilling(ctx, msg)); code != global.DefaultBilling {
		t.Errorf("an invalid carried code gave %q, want the default", code)
	}
}

// Messages carrying different billing codes are never merged into one turn.
func TestTakeBatch_SplitsByBillingCode(t *testing.T) {
	msg := func(id, code string) bus.InboundMessage {
		return bus.InboundMessage{
			Channel: "telegram", ChatID: "c", MessageID: id, Content: id,
			Metadata: bus.SetBilling(map[string]string{}, code),
		}
	}
	pending := []bus.InboundMessage{msg("1", ""), msg("2", ""), msg("3", "acme"), msg("4", "acme")}
	first, _ := takeBatch(&pending, false)
	if first.Content != "1\n2" || len(pending) != 2 {
		t.Fatalf("first batch %q, %d left", first.Content, len(pending))
	}
	second, _ := takeBatch(&pending, false)
	if second.Content != "3\n4" || second.Metadata[bus.MetaBilling] != "acme" {
		t.Fatalf("second batch %q billed %q", second.Content, second.Metadata[bus.MetaBilling])
	}
}

// An ask is billed to the asking work's code; a background result re-enters
// with the turn's code only when the turn inherited it; a relaunched task's
// result with the code it recorded.
func TestBillingCarriedOnMessages(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	if err := al.setSessionBilling(main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	scoped := hook.WithScope(context.Background(), hook.Scope{AgentID: "main", Session: mainSession})
	if err := al.publishAsk(scoped, "ask-1", sender{name: "Alice"}, main, "hi", nil, 0); err != nil {
		t.Fatal(err)
	}
	if in, ok := consumeInbound(t, tl.msgBus); !ok || in.Metadata[bus.MetaBilling] != "garden" {
		t.Errorf("ask carries %q, want garden", in.Metadata[bus.MetaBilling])
	}
	if err := al.publishAsk(tools.WithBilling(scoped, "acme"), "ask-2", sender{name: "Alice"}, main, "hi", nil, 0); err != nil {
		t.Fatal(err)
	}
	if in, _ := consumeInbound(t, tl.msgBus); in.Metadata[bus.MetaBilling] != "acme" {
		t.Errorf("inherited ask carries %q, want acme", in.Metadata[bus.MetaBilling])
	}

	opts := processOptions{Channel: "telegram", ChatID: "c1", SessionKey: mainSession}
	al.asyncToolCallback(scoped, main, opts, "slow")(scoped, &toolkit.Result{ForLLM: "result"})
	if in, ok := consumeInbound(t, tl.msgBus); !ok {
		t.Fatal("no background result")
	} else if _, has := in.Metadata[bus.MetaBilling]; has {
		t.Errorf("result of a session-billed turn carries %q", in.Metadata[bus.MetaBilling])
	}
	inherited := tools.WithBilling(scoped, "acme")
	al.asyncToolCallback(inherited, main, opts, "slow")(inherited, &toolkit.Result{ForLLM: "result"})
	if in, _ := consumeInbound(t, tl.msgBus); in.Metadata[bus.MetaBilling] != "acme" {
		t.Errorf("result of an inherited turn carries %q, want acme", in.Metadata[bus.MetaBilling])
	}

	al.taskPointerCallback("telegram", "c1", "main", 0, "acme")(context.Background(), &toolkit.Result{ForLLM: "done"})
	if in, _ := consumeInbound(t, tl.msgBus); in.Metadata[bus.MetaBilling] != "acme" {
		t.Errorf("relaunched task's result carries %q, want acme", in.Metadata[bus.MetaBilling])
	}
}

// A turn's inherited code is recorded with its pending turn, kept up to date
// when the turn changes it, and replayed after a restart.
func TestPendingTurn_Billing(t *testing.T) {
	tl, store := newRecoveryTestLoop(t)
	main := mustGetAgent(t, tl.al)
	ctx := tools.WithBilling(context.Background(), "acme")
	opts := processOptions{SessionKey: testSessionKey, Channel: "webui", ChatID: "chat-7", MessageID: "m1"}
	tl.al.recordPendingTurnSource(ctx, main, opts)
	sm := testStateManager(t, tl.al, "main")
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Billing != "acme" {
		t.Fatalf("pending turn billed %q, want acme", pt.Billing)
	}
	tools.InheritedBillingOf(ctx).Set("acme-2")
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Billing != "acme-2" {
		t.Fatalf("pending turn billed %q after the turn changed it, want acme-2", pt.Billing)
	}
	// A later turn of the session is not overwritten by this one's change.
	if err := sm.SetPendingTurn(testSessionKey, state.PendingTurn{Channel: "webui", ChatID: "chat-7", MessageID: "m2"}); err != nil {
		t.Fatal(err)
	}
	tools.InheritedBillingOf(ctx).Set("acme-3")
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Billing != "" {
		t.Fatalf("another turn's record billed %q", pt.Billing)
	}

	if err := sm.SetPendingTurn(testSessionKey, state.PendingTurn{Channel: "webui", ChatID: "chat-7", Billing: "acme-2"}); err != nil {
		t.Fatal(err)
	}
	tl.al.recoverSession(context.Background(), "main", testSessionKey, store)
	consumeOutbound(t, tl.msgBus) // the replay notice
	msg, ok := consumeInbound(t, tl.msgBus)
	if !ok || msg.Metadata[bus.MetaBilling] != "acme-2" {
		t.Fatalf("replay carries %q, want acme-2", msg.Metadata[bus.MetaBilling])
	}

	// A turn billed to its session's code records none.
	tl.al.recordPendingTurnSource(context.Background(), main, opts)
	if pt, _ := sm.GetPendingTurn(testSessionKey); pt.Billing != "" {
		t.Errorf("session-billed turn recorded %q", pt.Billing)
	}
}

// A forum participant's turn is billed to the code the run was launched
// under; a run launched before billing codes to the default.
func TestForumHost_AskBilling(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	rec := &billingRecorder{}
	r := newForumRig(t, forumConfig(t, true, false), rec)
	for _, tc := range []struct{ origin, want string }{{"acme", "acme"}, {"", global.DefaultBilling}, {"not a code", global.DefaultBilling}} {
		ctx := forum.WithAskInfo(context.Background(), forum.AskInfo{ForumID: "f1", Origin: forum.Origin{AgentID: "alice", Billing: tc.origin}})
		before := len(rec.seen())
		if _, err := r.host.Ask(ctx, "bob", "hello", 10*time.Second); err != nil {
			t.Fatalf("Ask: %v", err)
		}
		got := rec.seen()
		if len(got) != before+1 || got[len(got)-1] != tc.want {
			t.Errorf("origin billed %q: participant's call billed %v, want %q", tc.origin, got, tc.want)
		}
	}
}

// Summaries of a conversation are billed to its code; memory consolidation
// to the memory code; a vision describe call to the turn's.
func TestSummaryVisionAndMemoryBilling(t *testing.T) {
	tl, main := billingLoop(t)
	al := tl.al
	if err := al.setSessionBilling(main, mainSession, "garden"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	summary, _ := al.newCompressModelCaller(main, mainSession, models.Summarize)
	if got := summary.billing(ctx); got != "garden" {
		t.Errorf("summary billed %q, want garden", got)
	}
	if got := summary.billing(tools.WithBilling(ctx, "acme")); got != "acme" {
		t.Errorf("summary in inherited work billed %q, want acme", got)
	}
	rec := &billingRecorder{}
	setTestProvider(main, rec)
	memoryCaller, ok := al.NewMemoryModelCaller(main).(*memoryModelCaller)
	if !ok {
		t.Fatal("not the host's memory caller")
	}
	if _, _, _, err := memoryCaller.complete(ctx, "sys", "user", false, nil); err != nil {
		t.Fatalf("memory call: %v", err)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != global.MemoryBilling {
		t.Errorf("memory consolidation billed %v, want [memory]", got)
	}

	vc := &providerVisionClient{
		catalog: al.catalog, alias: "test-model", model: "test-model", id: main.Identity(),
		billing: func(ctx context.Context) string { return al.billingFor(ctx, main, mainSession) },
	}
	if _, err := vc.Complete(tools.WithBilling(ctx, "acme"), []spawnllm.Message{{Role: "user", Content: "look"}}); err != nil {
		t.Fatalf("vision call: %v", err)
	}
	if got := rec.seen(); len(got) != 2 || got[1] != "acme" {
		t.Errorf("vision call billed %v, want the second to acme", got)
	}
}

// A task-model call is billed to the code on its task's context.
func TestTaskModelHost_BillsTheTaskCode(t *testing.T) {
	recs := &accountingRecords{}
	accounting.SetLogger(slog.New(recs))
	t.Cleanup(func() { accounting.SetLogger(nil) })
	e := newTaskHostEnv(t)
	e.host = &taskModelHost{al: e.al}
	if d := catalogDigest(e.cfg); d != e.al.catalogDigest {
		e.al.adoptCatalog(e.al.catalogs.Build(e.cfg), d)
	}
	if _, err := e.host.Run(tools.WithBilling(context.Background(), "acme"), "alice", e.workspace,
		toolsagents.TaskModelRequest{Kind: "text", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	got := recs.byKind("text")
	if len(got) != 1 || got[0]["project"] != "acme" || got[0]["agent"] != "alice" {
		t.Errorf("records %v, want one for alice billed to acme", got)
	}
}

// A classifier's model call is billed to the classifier's code; one created
// before billing codes to the default.
func TestClassifierHost_CompleteBilling(t *testing.T) {
	recs := &accountingRecords{}
	accounting.SetLogger(slog.New(recs))
	t.Cleanup(func() { accounting.SetLogger(nil) })
	srv := &chatServer{reply: "urgent\nfrom a client"}
	ts := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer ts.Close()
	_, host, _ := newClassifierHostLoop(t, classifierHostConfig(t, ts.URL))
	for _, code := range []string{"acme", "", "not a code"} {
		if _, _, err := host.Complete(context.Background(), "alice", code, "Sort it.", "<item>hi</item>"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	got := recs.byKind("text")
	if len(got) != 3 || got[0]["project"] != "acme" || got[1]["project"] != global.DefaultBilling || got[2]["project"] != global.DefaultBilling {
		t.Errorf("records %v, want acme, then default twice", got)
	}
}

// The billing tool, which every agent has, does not count as a tool for the
// notice an agent without tools gets.
func TestWorkToolCount_IgnoresBilling(t *testing.T) {
	tl, main := billingLoop(t)
	reg := tools.NewToolRegistry()
	if workToolCount(reg) != 0 {
		t.Fatal("empty registry has tools")
	}
	saved := main.Tools
	main.Tools = reg
	t.Cleanup(func() { main.Tools = saved })
	withBillingTool(t, tl.al, main)
	if n := workToolCount(reg); n != 0 {
		t.Errorf("billing alone counts %d tools", n)
	}
	reg.Register(&mockEchoTool{})
	if n := workToolCount(reg); n != 1 {
		t.Errorf("billing and one tool count %d", n)
	}
}

// billingTool records the inherited billing code of its call.
type billingTool struct{ code *string }

func (b billingTool) Name() string               { return "inbox_list" }
func (b billingTool) Description() string        { return "records its billing code" }
func (b billingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (b billingTool) Execute(ctx context.Context, _ map[string]any) *toolkit.Result {
	*b.code, _ = tools.Billing(ctx)
	return toolkit.NewResult("[]")
}

// A classifier's tool call runs under the classifier's code (checked), so a
// tool that starts model work bills it there.
func TestClassifierHost_CallBilling(t *testing.T) {
	al, host, _ := newClassifierHostLoop(t, classifierHostConfig(t, "http://127.0.0.1:0"))
	var seen string
	registerOn(t, al, "alice", billingTool{code: &seen})
	for _, tc := range []struct{ code, want string }{{"acme", "acme"}, {"", global.DefaultBilling}, {"not a code", global.DefaultBilling}} {
		if _, err := host.Call(classifier.WithBilling(context.Background(), tc.code), "alice", "inbox_list", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if seen != tc.want {
			t.Errorf("classifier billed %q: tool call billed %q, want %q", tc.code, seen, tc.want)
		}
	}
}

// A deleted session's billing code is forgotten with it, whichever path
// deletes it (retention, erase, a single-shot discard): a new conversation
// under the same key starts at the default.
func TestSessionBilling_ForgottenWithTheSession(t *testing.T) {
	t.Run("retention", func(t *testing.T) {
		tl, main := billingLoop(t)
		tl.cfg.Session.RetentionDays = 30
		const key = "agent:main:telegram:direct:1"
		seedRetentionArchive(t, filepath.Join(main.Workspace, "sessions"), key, time.Now().Add(-31*24*time.Hour), false)
		tl.al.sessionBilling.Store(activeModelCacheKey(main.ID, key), "garden")
		if rep := tl.al.runRetentionPass(time.Now()); len(rep.deleted) != 1 {
			t.Fatalf("deleted %v", rep.deleted)
		}
		if got := tl.al.billingFor(context.Background(), main, key); got != global.DefaultBilling {
			t.Errorf("after retention = %q, want the default", got)
		}
	})
	t.Run("erase", func(t *testing.T) {
		tl, main := billingLoop(t)
		const key = "agent:main:telegram:direct:2"
		if err := tl.al.setSessionBilling(main, key, "garden"); err != nil {
			t.Fatal(err)
		}
		if err := tl.al.ReleaseSession(key); err != nil {
			t.Fatal(err)
		}
		if err := memory.DeleteSession(main.SessionsDir(), key); err != nil {
			t.Fatal(err)
		}
		if got := tl.al.billingFor(context.Background(), main, key); got != global.DefaultBilling {
			t.Errorf("after erase = %q, want the default", got)
		}
	})
	t.Run("single-shot discard", func(t *testing.T) {
		tl, main := billingLoop(t)
		const key = "agent:main:telegram:direct:3"
		if err := tl.al.setSessionBilling(main, key, "garden"); err != nil {
			t.Fatal(err)
		}
		tl.al.discardConversation(context.Background(), main, key)
		if got := tl.al.billingFor(context.Background(), main, key); got != global.DefaultBilling {
			t.Errorf("after a discard = %q, want the default", got)
		}
	})
}
