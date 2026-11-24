// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/commands"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/msgtoken"
)

// cmdEdgeNextInbound returns the next inbound message on msgBus, or fails.
func cmdEdgeNextInbound(t *testing.T, msgBus *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("no inbound message")
	}
	return msg
}

// cmdEdgeLoop is a loop over the test config with its bus and default agent.
func cmdEdgeLoop(t *testing.T, cfg *config.Config) (*AgentLoop, *bus.MessageBus, *AgentInstance) {
	t.Helper()
	msgBus := bus.NewMessageBus()
	al := mustNewAgentLoop(t, cfg, msgBus, &mockProvider{}, nil)
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	return al, msgBus, agent
}

// cmdEdgeTruncateFails is a session store whose history cannot be truncated.
type cmdEdgeTruncateFails struct{ session.SessionStore }

func (cmdEdgeTruncateFails) TruncateHistory(string, int) error { return errors.New("disk full") }

// TestHandleCommand_Replies: what a chat gets for text that is not a
// command, for a command when the loop has no command registry, for an
// unknown command, for a bare "/" and for a command that replies nothing.
func TestHandleCommand_Replies(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al, _, agent := cmdEdgeLoop(t, newTestConfig(t))
	opts := &processOptions{SessionKey: "agent:main:cmd"}
	setHistory(t, agent.Sessions, opts.SessionKey, []spawnllm.Message{{Role: "user", Content: "hello"}})

	for _, tc := range []struct {
		name, text, want string
		handled          bool
	}{
		{"not a command", "hello there", "", false},
		{"unknown command", "/nosuch arg", "Unknown command: /nosuch. Type /help for available commands.", true},
		{"no command name", "/", "Unknown command. Type /help for available commands.", true},
		{"no reply", "/retry", "", true},
		{"handler reply", "/clear", "Context cleared, but long-term memory preserved.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, handled := al.handleCommand(t.Context(), inbound("c1", "m1", tc.text), agent, opts)
			if got != tc.want || handled != tc.handled {
				t.Fatalf("handleCommand(%q) = %q, %v; want %q, %v", tc.text, got, handled, tc.want, tc.handled)
			}
		})
	}

	// A handler's error is reported naming the command.
	al.cmdRegistry = commands.NewRegistry([]commands.Definition{{
		Name: "boom",
		Handler: func(context.Context, commands.Request, *commands.Runtime) error {
			return errors.New("store unavailable")
		},
	}})
	if got, handled := al.handleCommand(t.Context(), inbound("c1", "m1", "/boom"), agent, opts); got != "Failed to execute /boom: store unavailable" || !handled {
		t.Fatalf("a failing handler = %q, %v", got, handled)
	}

	al.cmdRegistry = nil
	for _, tc := range []struct{ text, want string }{
		{"/status", "Unknown command: /status"},
		{"/", "Unknown command."},
	} {
		if got, handled := al.handleCommand(t.Context(), inbound("c1", "m1", tc.text), agent, opts); got != tc.want || !handled {
			t.Fatalf("without a registry, %q = %q, %v; want %q, true", tc.text, got, handled, tc.want)
		}
	}
}

// TestCommandsRuntime_NoAgent: without an agent the runtime offers only
// what every command may use, and its session hooks report nothing.
func TestCommandsRuntime_NoAgent(t *testing.T) {
	al, _, _ := cmdEdgeLoop(t, newTestConfig(t))
	rt := al.buildCommandsRuntime(t.Context(), nil, nil, inbound("c1", "m1", "/status"))

	if rt.AgentName != "" || rt.GetModelInfo != nil || rt.ClearHistory != nil || rt.ListTokenQuota != nil || rt.GetContextWindow != nil {
		t.Fatal("agent hooks must be absent without an agent")
	}
	if got := rt.GetSessionChannels(); got != nil {
		t.Fatalf("GetSessionChannels = %v, want nil", got)
	}
	if n, first, last := rt.GetArchiveStats(); n != 0 || !first.IsZero() || !last.IsZero() {
		t.Fatalf("GetArchiveStats = %d %v %v, want zero", n, first, last)
	}
	if got := rt.GetMemoryStatus(); got != "" {
		t.Fatalf("GetMemoryStatus = %q, want empty", got)
	}
	if m, tok, sum := rt.GetSessionStats(); m != 0 || tok != 0 || sum != 0 {
		t.Fatalf("GetSessionStats = %d %d %d, want zero", m, tok, sum)
	}
	if got := rt.GetEnabledChannels(); got != nil {
		t.Fatalf("GetEnabledChannels without a channel manager = %v, want nil", got)
	}
	if err := rt.SwitchChannel("test"); err == nil || err.Error() != "channel manager not initialized" {
		t.Fatalf("SwitchChannel without a channel manager = %v", err)
	}
	if rt.Uptime() < 0 {
		t.Fatal("Uptime must not be negative")
	}
	if !slices.Contains(rt.ListAgentIDs(), "main") || len(rt.ListDefinitions()) == 0 {
		t.Fatal("the runtime must list the agents and the command definitions")
	}
	if got := rt.AskAgent(t.Context(), "zed", "hi"); got != "There is no agent named zed." {
		t.Fatalf("AskAgent = %q", got)
	}
	if got := rt.WhisperAgent(t.Context(), "zed", "hi"); got != "There is no agent named zed." {
		t.Fatalf("WhisperAgent = %q", got)
	}
}

// TestCommandsRuntime_Channels: /list channels names the enabled channels,
// and /check channel accepts an enabled channel and "cli" and refuses any
// other.
func TestCommandsRuntime_Channels(t *testing.T) {
	al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
	cm, err := channels.NewManager(&config.Config{}, msgBus, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cm.RegisterChannel("test", &echoChannel{})
	al.SetChannelManager(cm)

	rt := al.buildCommandsRuntime(t.Context(), agent, nil, inbound("c1", "m1", "/list channels"))
	if got := rt.GetEnabledChannels(); !slices.Equal(got, []string{"test"}) {
		t.Fatalf("GetEnabledChannels = %v, want [test]", got)
	}
	for _, ch := range []string{"test", "cli"} {
		if err := rt.SwitchChannel(ch); err != nil {
			t.Fatalf("SwitchChannel(%s) = %v, want nil", ch, err)
		}
	}
	if err := rt.SwitchChannel("bob"); err == nil || err.Error() != "channel 'bob' not found or not enabled" {
		t.Fatalf("SwitchChannel(bob) = %v", err)
	}
}

// TestCommandsRuntime_SessionHooks: with an agent and a session, the runtime
// reports the agent's channels, the session's archive, memory, history and
// context window, and reads and sets the session's switches.
func TestCommandsRuntime_SessionHooks(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Bindings = []config.AgentBinding{
		{AgentID: "main", Match: config.BindingMatch{Channel: "telegram"}},
		{AgentID: "bob", Match: config.BindingMatch{Channel: "slack"}},
	}
	al, _, agent := cmdEdgeLoop(t, cfg)
	opts := &processOptions{SessionKey: "agent:main:hooks"}

	cm, release := al.getContextManager(agent, opts.SessionKey)
	for _, text := range []string{"hello Bob", "how are you"} {
		if _, err := cm.AddUserMessage(context.Background(), spawnllm.Message{Role: "user", Content: text}); err != nil {
			release()
			t.Fatal(err)
		}
	}
	release()
	setSummary(t, agent.Sessions, opts.SessionKey, "summary")

	rt := al.buildCommandsRuntime(t.Context(), agent, opts, inbound("c1", "m1", "/status"))
	if rt.AgentName != "Main" {
		t.Fatalf("AgentName = %q, want Main", rt.AgentName)
	}
	if got := rt.GetSessionChannels(); !slices.Equal(got, []string{"telegram"}) {
		t.Fatalf("GetSessionChannels = %v, want [telegram]", got)
	}
	n, first, last := rt.GetArchiveStats()
	if n != 2 || first.IsZero() || last.Before(first) {
		t.Fatalf("GetArchiveStats = %d %v %v, want two messages", n, first, last)
	}
	if got, want := rt.GetMemoryStatus(), al.cogmemSessionStatus(t.Context(), agent, opts.SessionKey); got == "" || got != want {
		t.Fatalf("GetMemoryStatus = %q, want %q", got, want)
	}
	if m, tok, sum := rt.GetSessionStats(); m != 2 || tok <= 0 || sum != len("summary") {
		t.Fatalf("GetSessionStats = %d %d %d, want 2 messages, some tokens, 7 summary chars", m, tok, sum)
	}
	if got, want := rt.GetContextWindow(), al.sessionLimits(agent, opts.SessionKey).contextWindow; got != want {
		t.Fatalf("GetContextWindow = %d, want %d", got, want)
	}

	if rt.GetExposeReasoning() || rt.GetShowToolActivity() {
		t.Fatal("the switches must start off")
	}
	rt.SetExposeReasoning(true)
	rt.SetShowToolActivity(true)
	if !rt.GetExposeReasoning() || !rt.GetShowToolActivity() {
		t.Fatal("the switches must be on once set")
	}
	if !al.getExposeReasoning(agent, opts.SessionKey) || !al.getShowToolActivity(agent, opts.SessionKey) {
		t.Fatal("the switches must be set on the session")
	}

	o := rt.CancelPending(true)
	if !o.All || o.Agent != agent.Identity().Name || o.Cancelled != 0 || o.Running || o.Skipped != 0 {
		t.Fatalf("CancelPending(all) = %+v, want nothing cancelled for Main", o)
	}
}

// TestCommandsRuntime_NoSession: with an agent but no session, the session
// hooks refuse or report the defaults and change nothing.
func TestCommandsRuntime_NoSession(t *testing.T) {
	al, agent := fourModelLoop(t)
	rt := al.buildCommandsRuntime(t.Context(), agent, nil, inbound("c1", "m1", "/model"))

	if got := rt.GetContextWindow(); got != agent.ContextWindow {
		t.Fatalf("GetContextWindow = %d, want the agent's %d", got, agent.ContextWindow)
	}
	if n, _, _ := rt.GetArchiveStats(); n != 0 {
		t.Fatalf("GetArchiveStats = %d, want 0", n)
	}
	if got := rt.GetMemoryStatus(); got != "" {
		t.Fatalf("GetMemoryStatus = %q, want empty", got)
	}
	rt.SetExposeReasoning(true)
	rt.SetShowToolActivity(true)
	if rt.GetExposeReasoning() || rt.GetShowToolActivity() {
		t.Fatal("without a session the switches stay off")
	}
	if _, err := rt.SetActiveModel(1); err == nil || err.Error() != "process options not available" {
		t.Fatalf("SetActiveModel = %v", err)
	}
	if entries, active := rt.GetAgentModels(); len(entries) != 4 || active != 0 {
		t.Fatalf("GetAgentModels = %v, %d; want four models, the first active", entries, active)
	}
	if _, err := rt.CompactHistory(t.Context()); err == nil || err.Error() != "process options not available" {
		t.Fatalf("CompactHistory = %v", err)
	}
	if err := rt.ClearHistory(); err == nil || err.Error() != "process options not available" {
		t.Fatalf("ClearHistory = %v", err)
	}
	if rt.CancelPending != nil {
		t.Fatal("CancelPending needs a session")
	}
	if err := rt.RetriggerLastMessage(t.Context()); err == nil || err.Error() != "session not available" {
		t.Fatalf("RetriggerLastMessage = %v", err)
	}
}

// TestCommandsRuntime_SetActiveModel: /model <n> selects the n-th model and
// refuses an index out of range.
func TestCommandsRuntime_SetActiveModel(t *testing.T) {
	al, agent := fourModelLoop(t)
	opts := &processOptions{SessionKey: "s"}
	rt := al.buildCommandsRuntime(t.Context(), agent, opts, inbound("c1", "m1", "/model"))

	if _, err := rt.SetActiveModel(4); err == nil || err.Error() != "model index out of range (0-3)" {
		t.Fatalf("SetActiveModel(4) = %v", err)
	}
	name, err := rt.SetActiveModel(2)
	if err != nil || name != "a2" {
		t.Fatalf("SetActiveModel(2) = %q, %v; want a2", name, err)
	}
	if _, active := rt.GetAgentModels(); active != 2 {
		t.Fatalf("active model = %d, want 2", active)
	}
}

// TestArchiveStats_NoArchive: a session with no archive has no messages.
func TestArchiveStats_NoArchive(t *testing.T) {
	n, first, last := archiveStats(filepath.Join(t.TempDir(), "missing", "archive.db"))
	if n != 0 || !first.IsZero() || !last.IsZero() {
		t.Fatalf("archiveStats = %d %v %v, want zero", n, first, last)
	}
}

// TestClearHistory_Outcomes: /clear refuses a session without a store and
// reports a failed reset; a person's session is cleared without the notice
// an agent gets; a notice that cannot be published is logged.
func TestClearHistory_Outcomes(t *testing.T) {
	t.Run("no session store", func(t *testing.T) {
		al, _, agent := cmdEdgeLoop(t, newTestConfig(t))
		bare := &AgentInstance{ID: agent.ID}
		err := al.clearHistory(bare, &processOptions{SessionKey: "s"}, inbound("c1", "m1", "/clear"))
		if err == nil || err.Error() != "sessions not initialized for agent" {
			t.Fatalf("clearHistory = %v", err)
		}
	})

	t.Run("reset fails", func(t *testing.T) {
		logs := &safeBufLoop{}
		t.Cleanup(logger.RedirectForTest(logs))
		al, _, agent := cmdEdgeLoop(t, newTestConfig(t))
		agent.Sessions = cmdEdgeTruncateFails{SessionStore: agent.Sessions}
		err := al.clearHistory(agent, &processOptions{SessionKey: "agent:main:resetfail"}, inbound("c1", "m1", "/clear"))
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("clearHistory = %v, want the reset error", err)
		}
		if !strings.Contains(logs.String(), "clear: Reset failed") {
			t.Fatalf("the failed reset was not logged:\n%s", logs.String())
		}
	})

	t.Run("agent gets a notice", func(t *testing.T) {
		al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
		const key = "agent:main:notice"
		setHistory(t, agent.Sessions, key, []spawnllm.Message{{Role: "user", Content: "hello"}})
		msg := inbound("c1", "m1", "/clear")
		if err := al.clearHistory(agent, &processOptions{SessionKey: key}, msg); err != nil {
			t.Fatal(err)
		}
		if got := agent.Sessions.GetHistory(key); len(got) != 0 {
			t.Fatalf("history after /clear = %v, want empty", got)
		}
		n := cmdEdgeNextInbound(t, msgBus)
		if n.Content != wrapClearNotice("") || n.SessionKey != key || !n.Internal || n.SenderID != "system" ||
			n.Channel != "test" || n.ChatID != "c1" || n.Metadata[metadataKeyPreresolvedAgentID] != agent.ID {
			t.Fatalf("notice = %+v", n)
		}
	})

	t.Run("person gets no notice", func(t *testing.T) {
		al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
		agent.HumanModel = "bob-person"
		if err := al.clearHistory(agent, &processOptions{SessionKey: "agent:main:person"}, inbound("c1", "m1", "/clear")); err != nil {
			t.Fatal(err)
		}
		// clearHistory returns before it would start publishing: the next
		// message on the bus is the test's own.
		if err := msgBus.PublishInbound(t.Context(), inbound("c1", "m2", "sentinel")); err != nil {
			t.Fatal(err)
		}
		if msg := cmdEdgeNextInbound(t, msgBus); msg.Content != "sentinel" {
			t.Fatalf("a person's /clear published %+v", msg)
		}
	})

	t.Run("notice not published", func(t *testing.T) {
		logs := &safeBufLoop{}
		t.Cleanup(logger.RedirectForTest(logs))
		al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
		msgBus.Close()
		if err := al.clearHistory(agent, &processOptions{SessionKey: "agent:main:closed"}, inbound("c1", "m1", "/clear")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the failed notice to be logged", func() bool {
			return strings.Contains(logs.String(), "clear: failed to publish clear notice")
		})
	})
}

// TestRetriggerLastMessage: /retry sends the session's last user message
// again, marked as a retry, to the chat it came from; it refuses when there
// is nothing to retry, and logs a message it could not send.
func TestRetriggerLastMessage(t *testing.T) {
	t.Run("resends the last user message", func(t *testing.T) {
		al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
		const key = "agent:main:retry"
		setHistory(t, agent.Sessions, key, []spawnllm.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "answer"},
			{Role: "user", Content: "second"},
			{Role: "user", Content: ""},
			{Role: "assistant", Content: "failed"},
		})
		msg := inbound("c1", "m1", "/retry")
		msg.Peer = bus.Peer{Kind: "direct", ID: "u1"}
		if err := al.retriggerLastMessage(t.Context(), agent, &processOptions{SessionKey: key}, msg); err != nil {
			t.Fatal(err)
		}
		got := cmdEdgeNextInbound(t, msgBus)
		if got.Content != "second" || !got.IsRetry || !got.Internal || got.Channel != "test" ||
			got.ChatID != "c1" || got.SenderID != "u1" || got.Peer != msg.Peer {
			t.Fatalf("retry = %+v", got)
		}
	})

	t.Run("nothing to retry", func(t *testing.T) {
		al, _, agent := cmdEdgeLoop(t, newTestConfig(t))
		const key = "agent:main:empty"
		setHistory(t, agent.Sessions, key, []spawnllm.Message{{Role: "assistant", Content: "hi"}})
		err := al.retriggerLastMessage(t.Context(), agent, &processOptions{SessionKey: key}, inbound("c1", "m1", "/retry"))
		if err == nil || err.Error() != "no previous message to retry" {
			t.Fatalf("retriggerLastMessage = %v", err)
		}
	})

	t.Run("not sent", func(t *testing.T) {
		logs := &safeBufLoop{}
		t.Cleanup(logger.RedirectForTest(logs))
		al, msgBus, agent := cmdEdgeLoop(t, newTestConfig(t))
		const key = "agent:main:closed"
		setHistory(t, agent.Sessions, key, []spawnllm.Message{{Role: "user", Content: "again"}})
		msgBus.Close()
		if err := al.retriggerLastMessage(t.Context(), agent, &processOptions{SessionKey: key}, inbound("c1", "m1", "/retry")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the failed retry to be logged", func() bool {
			return strings.Contains(logs.String(), "Failed to retrigger message after /retry")
		})
	})
}

// TestCommandsRuntime_TokenQuota: /quota lists this agent's named message
// tokens with their limits and blocks, and /quota reset clears the blocks.
func TestCommandsRuntime_TokenQuota(t *testing.T) {
	al, _, agent := cmdEdgeLoop(t, newTestConfig(t))
	named, err := msgtoken.NewNamedStore("")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := named.Create(agent.ID, "gps")
	if err != nil {
		t.Fatal(err)
	}
	named.Update(agent.ID, tok.ID, 1, 15)
	if _, err := named.Create("bob", "other"); err != nil {
		t.Fatal(err)
	}
	al.mu.Lock()
	al.namedTokens = named
	al.mu.Unlock()
	for range 2 { // the second check is over the limit of one a minute
		al.CheckMessageToken(tok.Token)
	}

	rt := al.buildCommandsRuntime(t.Context(), agent, nil, inbound("c1", "m1", "/quota"))
	got := rt.ListTokenQuota()
	if len(got) != 1 {
		t.Fatalf("ListTokenQuota = %+v, want only Main's token", got)
	}
	q := got[0]
	if q.Name != "gps" || q.RatePerMin != 1 || q.BlockMinutes != 15 || !q.Blocked || q.BlockRemaining <= 0 {
		t.Fatalf("quota = %+v, want gps blocked at 1/min for 15 minutes", q)
	}
	if n := rt.ResetTokenQuota(""); n != 1 {
		t.Fatalf("ResetTokenQuota = %d, want 1 block cleared", n)
	}
	if got := rt.ListTokenQuota(); got[0].Blocked {
		t.Fatalf("after reset = %+v, want unblocked", got[0])
	}
}

// TestSessionChannelsForAgent_SkipsBindingsWithoutChannel: a binding to the
// agent that names no channel adds none, and channels are listed once.
func TestSessionChannelsForAgent_SkipsBindingsWithoutChannel(t *testing.T) {
	got := sessionChannelsForAgent([]config.AgentBinding{
		{AgentID: "alice", Match: config.BindingMatch{Channel: "  "}},
		{AgentID: "alice", Match: config.BindingMatch{Channel: "Telegram"}},
		{AgentID: "bob", Match: config.BindingMatch{Channel: "slack"}},
		{AgentID: "Alice", Match: config.BindingMatch{Channel: "telegram"}},
	}, "alice")
	if !slices.Equal(got, []string{"Telegram"}) {
		t.Fatalf("sessionChannelsForAgent = %v, want [Telegram]", got)
	}
}
