// ClawEh
// License: MIT

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
)

// toolsEdgeLoop returns a test loop, its default agent and a captured log.
func toolsEdgeLoop(t *testing.T) (*testLoop, *AgentInstance, *safeBufLoop) {
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

// toolsEdgeWait fails the test when ch does not deliver within a generous bound.
func toolsEdgeWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// With discovery on, suite tools are hidden behind search_tools except the
// pinned Maestro entry tool, and a suite tool the agent denies is not
// registered at all.
func TestLoopTools_DiscoveryHidesSuiteToolsAndHonoursDeny(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := toolRegTestConfig(t)
	cfg.Tools.Discovery.Enabled = true
	cfg.Agents.List[0].Maestro = &config.MaestroConfig{Enabled: true}
	cfg.Agents.List[0].DenyTools = []string{"maestro_project_delete"}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent, ok := al.GetRegistry().Get("main")
	if !ok {
		t.Fatal("agent main missing")
	}

	if _, ok := agent.Tools.Get("maestro_project_delete"); ok {
		t.Error("a denied suite tool was registered")
	}
	if _, ok := agent.Tools.Get("maestro_project_create"); !ok {
		t.Fatal("maestro_project_create not registered")
	}
	deferred := map[string]bool{}
	sent := map[string]bool{}
	for _, d := range sentToolList(agent) {
		sent[d.Function.Name] = true
		deferred[d.Function.Name] = d.Deferred
	}
	if !sent["search_tools"] {
		t.Error("search_tools not offered with discovery on")
	}
	if !deferred["maestro_project_create"] {
		t.Error("maestro_project_create should be hidden behind discovery")
	}
	if !sent[global.MaestroEntryTool] || deferred[global.MaestroEntryTool] {
		t.Errorf("%s should stay listed", global.MaestroEntryTool)
	}
	if deferred["file_read_lines"] {
		t.Error("a native tool was hidden")
	}
}

func TestLoopTools_MessageToolOutcomes(t *testing.T) {
	tl, _, _ := toolsEdgeLoop(t)
	mt := tl.al.newMessageTool()
	ctx := tools.WithReplyChat(context.Background(), tools.ReplyChat{Channel: "telegram", ChatID: "c1"})

	// The channel reports the delivery.
	go func() {
		msg, ok := tl.msgBus.SubscribeOutbound(context.Background())
		if ok && msg.OnDelivery != nil {
			msg.OnDelivery(nil)
			msg.OnDelivery(errors.New("a second report is ignored"))
		}
	}()
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); res.IsError || !strings.Contains(res.ForLLM, "delivered") {
		t.Errorf("delivered send = %+v", res)
	}

	// The channel reports a failure.
	go func() {
		msg, ok := tl.msgBus.SubscribeOutbound(context.Background())
		if ok && msg.OnDelivery != nil {
			msg.OnDelivery(errors.New("recipient offline"))
		}
	}()
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); !res.IsError || res.Err == nil || !strings.Contains(res.Err.Error(), "recipient offline") {
		t.Errorf("failed send = %+v", res)
	}

	// No report before the turn's context ends: queued.
	qctx, cancel := context.WithCancel(ctx)
	go func() {
		if _, ok := tl.msgBus.SubscribeOutbound(context.Background()); ok {
			cancel()
		}
	}()
	if res := mt.Execute(qctx, map[string]any{"content": "hi"}); res.IsError || !strings.Contains(res.ForLLM, "queued") {
		t.Errorf("queued send = %+v", res)
	}

	// The bus is closed: not sent.
	tl.msgBus.Close()
	if res := mt.Execute(ctx, map[string]any{"content": "hi"}); !res.IsError {
		t.Errorf("send on a closed bus = %+v, want an error", res)
	}
}

func TestLoopTools_SpawnCandidatesAndCardCheck(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := cardsConfig(t) // cards live in the test's own data directory
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	agent := al.registry.Default()
	if c, ok := al.spawnCandidates("nobody"); ok || c != nil {
		t.Errorf("spawnCandidates(unknown) = %v, %v", c, ok)
	}
	if _, ok := al.spawnCandidates(agent.ID); !ok {
		t.Error("spawnCandidates(known) not found")
	}

	allowAll := func(string, string) bool { return true }
	spawner := al.newSpawner(agent, agent.ID, cfg, al.spawnCandidates, allowAll)
	res, err := spawner.Spawn(context.Background(), global.SpawnRequest{Task: "x", CharacterCard: "nope"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !res.IsError || !strings.Contains(res.ForLLM, `Character card "nope" does not exist.`) {
		t.Errorf("spawn with a missing card = %+v", res)
	}
}

func TestLoopTools_SessionClosures(t *testing.T) {
	tl, agent, _ := toolsEdgeLoop(t)
	ctx := context.Background()
	key := routing.BuildAgentMainSessionKey(agent.ID)

	info, err := tl.al.sessionInfoFn(agent)(ctx, key)
	if err != nil || info == nil || info.SessionKey != key {
		t.Fatalf("session info = %+v, %v", info, err)
	}

	for _, m := range [][2]string{{"user", "hello"}, {"assistant", "hi there"}} {
		if addErr := agent.Sessions.AddMessage(key, m[0], m[1]); addErr != nil {
			t.Fatal(addErr)
		}
	}
	// Two short messages: nothing to compress, said in the report and the error.
	report, summary, err := tl.al.compactFn(agent)(ctx, key)
	if err == nil || !strings.Contains(err.Error(), "nothing to compress") {
		t.Errorf("compact error = %v, want nothing to compress", err)
	}
	if !strings.Contains(report, "Nothing to compress") || summary != "" {
		t.Errorf("compact report = %q, summary = %q", report, summary)
	}
}

func TestLoopTools_ClearFn(t *testing.T) {
	tl, agent, _ := toolsEdgeLoop(t)
	clearSession := tl.al.clearFn(agent)

	ctx := toolkit.WithChat(context.Background(), "telegram", "c1")
	if err := clearSession(ctx, "s-peer", "carry on"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	in, ok := tl.msgBus.ConsumeInbound(context.Background())
	if !ok {
		t.Fatal("inbound bus closed")
	}
	if in.Channel != "telegram" || in.ChatID != "c1" || in.SenderID != "system" || !in.Internal ||
		in.SessionKey != "s-peer" || in.Peer.Kind != "channel" || in.Peer.ID != "c1" {
		t.Errorf("clear handoff = %+v", in)
	}
	if in.Metadata[metaSessionReset] != "true" || in.Metadata[metadataKeyPreresolvedAgentID] != agent.ID {
		t.Errorf("clear metadata = %v", in.Metadata)
	}
	if !strings.Contains(in.Content, "carry on") {
		t.Errorf("clear content = %q", in.Content)
	}

	// Cleared again at once: refused.
	if err := clearSession(ctx, "s-peer", "again"); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("second clear = %v, want rate-limited", err)
	}

	// A direct chat names no peer.
	dctx := toolkit.WithChat(context.Background(), "cli", "direct")
	if err := clearSession(dctx, "s-direct", "go"); err != nil {
		t.Fatalf("direct clear: %v", err)
	}
	in, _ = tl.msgBus.ConsumeInbound(context.Background())
	if in.Peer.Kind != "" || in.Peer.ID != "" {
		t.Errorf("direct clear peer = %+v, want none", in.Peer)
	}
}

// A replaced or deleted agent is not retired while one of its sessions is in
// use; once released it is, and another agent's sessions are left alone.
func TestLoopTools_RetireRefusedWhileSessionInUse(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := toolRegTestConfig(t)
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: "bob", Name: "Bob"})
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	reg := al.GetRegistry()
	main, _ := reg.Get("main")
	bob, ok := reg.Get("bob")
	if !ok {
		t.Fatal("agent bob missing")
	}
	mainKey := routing.BuildAgentMainSessionKey(main.ID)
	bobKey := routing.BuildAgentMainSessionKey(bob.ID)
	_, releaseBob := al.getContextManager(bob, bobKey)
	defer releaseBob()

	_, release := al.getContextManager(main, mainKey)
	if err := al.retireAgent(main.Spec, main); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Errorf("retire config agent in use = %v", err)
	}
	release()
	if err := al.retireAgent(main.Spec, main); err != nil {
		t.Errorf("retire after release = %v", err)
	}
	if _, ok := al.contextManagers.Load(bob.ID + ":" + bobKey); !ok {
		t.Error("retiring main evicted bob's session")
	}

	// A temporary agent whose session is in use is not deleted.
	cloneID, err := reg.CreateClone("bob")
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	clone, _ := reg.Get(cloneID)
	_, releaseClone := al.getContextManager(clone, routing.BuildAgentMainSessionKey(clone.ID))
	if err := al.retireAgent(clone.Spec, clone); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Errorf("retire temp agent in use = %v", err)
	}
	releaseClone()

	// Nothing to release on a store-less instance.
	if err := al.releaseReplacedInstance(&AgentInstance{}); err != nil {
		t.Errorf("releaseReplacedInstance(no store) = %v", err)
	}
}

// toolsEdgeEditChannel records placeholder edits and signals each one.
type toolsEdgeEditChannel struct {
	fakeChannel
	mu    sync.Mutex
	edits []string
	sig   chan string
}

func (c *toolsEdgeEditChannel) EditMessage(_ context.Context, _, _, content string) error {
	c.mu.Lock()
	c.edits = append(c.edits, content)
	c.mu.Unlock()
	select {
	case c.sig <- content:
	default:
	}
	return nil
}

var _ channels.MessageEditor = (*toolsEdgeEditChannel)(nil)

func TestLoopTools_TypingAndProgress(t *testing.T) {
	tl, _, _ := toolsEdgeLoop(t)
	cm, err := channels.NewManager(&config.Config{}, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := &toolsEdgeEditChannel{sig: make(chan string, 1)}
	cm.RegisterChannel("telegram", ch)
	cm.RecordPlaceholder("telegram", "c1", "ph-1")
	tl.al.SetChannelManager(cm)

	stopped := make(chan struct{}, 1)
	cm.RecordTypingStop("telegram", "c1", func() { stopped <- struct{}{} })
	tl.al.stopTyping("telegram", "c1")
	toolsEdgeWait(t, stopped, "typing to stop")

	var completed atomic.Int64
	completed.Store(3)
	stop := tl.al.startProgressUpdates(context.Background(), "telegram", "c1", time.Millisecond, &completed)
	got := toolsEdgeWait(t, ch.sig, "a progress edit")
	stop()
	if got != "⏳ Still working… 3 tool call(s) completed so far." {
		t.Errorf("progress edit = %q", got)
	}
}

func TestLoopTools_TaskPointerCallback(t *testing.T) {
	tl, _, logs := toolsEdgeLoop(t)
	ctx := context.Background()

	// Nothing to deliver.
	tl.al.taskPointerCallback("telegram", "c1", "main", 0)(ctx, nil)
	tl.al.taskPointerCallback("telegram", "c1", "main", 0)(ctx, &toolkit.Result{})

	// An asked turn's task drops its user output; the error text reaches the agent.
	tl.al.taskPointerCallback(constants.AgentMessageChannel, "c1", "", 1)(ctx,
		&toolkit.Result{ForUser: "dropped", Err: errors.New("task failed")})
	if out := execEdgeNextOutbound(t, tl.msgBus); out.Content != "execcov-marker" {
		t.Errorf("an asked task reached a chat: %q", out.Content)
	}
	in := execEdgeNextInbound(t, tl.msgBus)
	if in.Content != "task failed" || in.SessionKey != "" || in.Metadata[bus.MetaSpawnDepth] != "1" {
		t.Errorf("error-only pointer = %+v", in)
	}
	if _, ok := in.Metadata[metadataKeyPreresolvedAgentID]; ok {
		t.Error("an ownerless task named an agent")
	}

	// A closed bus: both publishes fail and are logged.
	tl.msgBus.Close()
	tl.al.taskPointerCallback("telegram", "c1", "main", 0)(ctx, &toolkit.Result{ForUser: "u", ForLLM: "m"})
	for _, want := range []string{"Failed to publish async task result to user", "Failed to publish async task result to agent"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

// The supervisor ticks on the loop's clock: an interrupted callback task is
// relaunched and its result reaches the owner's main conversation. Closing the
// loop stops the supervisor.
func TestLoopTools_SupervisorRelaunchesInterruptedTask(t *testing.T) {
	tl, agent, _ := toolsEdgeLoop(t)
	fc := clock.NewFake(time.Now())
	tl.al.clock = fc

	dir := filepath.Join(agent.Workspace, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-3333-4444-555555555555"
	rec := map[string]any{
		"uuid": id, "name": "relaunch", "owner_agent_id": agent.ID, "mode": "callback",
		"task": "say hi", "kind": "text", "channel": "telegram", "chat_id": "c1",
		"status": "running", "created_at": time.Now().UTC().Format(time.RFC3339),
		"results_path": "tasks/" + id + "-results.json", "spawn_depth": 1,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, id+"-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, id+".run"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tl.al.superviseTasks(context.Background())
	}()
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = fc.BlockUntil(waitCtx, 1); err != nil {
		t.Fatalf("supervisor never started its ticker: %v", err)
	}
	fc.Advance(global.TaskSupervisorInterval)

	in, ok := tl.msgBus.ConsumeInbound(waitCtx)
	if !ok {
		t.Fatal("no result from the relaunched task")
	}
	if in.Channel != "system" || in.SenderID != "async:agent_spawn" || in.ChatID != "c1" ||
		in.SessionKey != routing.BuildAgentMainSessionKey(agent.ID) ||
		in.Metadata[metadataKeyPreresolvedAgentID] != agent.ID ||
		in.Metadata[bus.MetaOriginChannel] != "telegram" || in.Metadata[bus.MetaSpawnDepth] != "1" {
		t.Errorf("relaunched task result = %+v", in)
	}
	if !strings.Contains(in.Content, "[TASK RESULT] Task 'relaunch' (uuid "+id) {
		t.Errorf("pointer content = %q", in.Content)
	}

	tl.al.Close(context.Background())
	select {
	case <-done:
	case <-waitCtx.Done():
		t.Fatal("supervisor still running after Close")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".run") }) {
		t.Error("run marker left after the relaunched task finished")
	}
}
