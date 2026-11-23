package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/tools"
)

// fakeAgents allows the targets listed in allow.
type fakeAgents struct {
	tools.AgentServices
	allow map[string]bool
}

func (f fakeAgents) CanTarget(id string) bool { return f.allow[id] }

// fakeMessenger records what it is asked to send.
type fakeMessenger struct {
	whispers []string // "fromID|from|to|message"
	asks     []string // "from|to|message|wait"
	reply    tools.AgentReply
	err      error
}

func (m *fakeMessenger) Ask(_ context.Context, from, agentID, message string, wait time.Duration) (tools.AgentReply, error) {
	m.asks = append(m.asks, strings.Join([]string{from, agentID, message, wait.String()}, "|"))
	return m.reply, m.err
}

func (m *fakeMessenger) Whisper(_ context.Context, fromID, from, agentID, message string) error {
	m.whispers = append(m.whispers, strings.Join([]string{fromID, from, agentID, message}, "|"))
	return m.err
}

func messageHost(m *fakeMessenger) tools.ToolDeps {
	cfg := &config.Config{}
	cfg.Agents.List = []config.AgentConfig{
		{ID: "alice", Name: "Alice"},
		{ID: "bob", Name: "Bob"},
		{ID: "carol", Name: "Carol"},
	}
	return tools.ToolDeps{
		Cfg:       cfg,
		AgentID:   "alice",
		Agents:    fakeAgents{allow: map[string]bool{"bob": true}},
		Messenger: m,
	}
}

func callMessage(host tools.ToolDeps, args map[string]any) *toolspec.Result {
	return runMessageTool(host, &toolspec.ToolCall{Ctx: context.Background(), Args: args})
}

// agent_message checks its arguments and the caller's allow list before it
// sends anything.
func TestMessageTool_Refusals(t *testing.T) {
	m := &fakeMessenger{}
	host := messageHost(m)
	long := strings.Repeat("x", tools.MaxAgentMessageChars+1)
	for _, tc := range []struct {
		name    string
		host    tools.ToolDeps
		args    map[string]any
		want    string
		refusal bool
	}{
		{"no host", tools.ToolDeps{}, map[string]any{"agent": "bob", "message": "hi", "wait_seconds": 0.0}, "agent_message is not available", false},
		{"no agent", host, map[string]any{"agent": " ", "message": "hi", "wait_seconds": 0.0}, "agent is required", false},
		{"no message", host, map[string]any{"agent": "bob", "message": " ", "wait_seconds": 0.0}, "message is required", false},
		{"too long", host, map[string]any{"agent": "bob", "message": long, "wait_seconds": 0.0}, tools.AgentMessageLimitText(), true},
		{"no wait", host, map[string]any{"agent": "bob", "message": "hi"}, "wait_seconds is required: 0 to whisper, or the seconds to wait for a reply", false},
		{"negative wait", host, map[string]any{"agent": "bob", "message": "hi", "wait_seconds": -1}, "wait_seconds is required", false},
		{"unknown agent", host, map[string]any{"agent": "dave", "message": "hi", "wait_seconds": int64(0)}, "There is no agent named dave.", true},
		{"not allowed", host, map[string]any{"agent": "Carol", "message": "hi", "wait_seconds": 0.0}, "You may not message Carol: it is not in your subagents.allow_agents.", true},
	} {
		res := callMessage(tc.host, tc.args)
		if !res.IsError || !strings.HasPrefix(res.ForLLM, tc.want) || toolkit.IsExpectedRefusal(res.Err) != tc.refusal {
			t.Errorf("%s: %+v", tc.name, res)
		}
	}
	if len(m.whispers)+len(m.asks) != 0 {
		t.Errorf("sent despite refusals: %v %v", m.whispers, m.asks)
	}
}

func TestMessageTool_WhisperAndAsk(t *testing.T) {
	m := &fakeMessenger{reply: tools.AgentReply{Outcome: bus.OutcomeOK, Text: "pong"}}
	host := messageHost(m)

	res := callMessage(host, map[string]any{"agent": "Bob", "message": "psst", "wait_seconds": 0.0})
	if res.IsError || res.ForLLM != "Whispered to Bob." || len(m.whispers) != 1 || m.whispers[0] != "alice|Alice|bob|psst" {
		t.Errorf("whisper: %+v %v", res, m.whispers)
	}
	res = callMessage(host, map[string]any{"agent": "bob", "message": "ping", "wait_seconds": 30})
	if res.IsError || res.ForLLM != "pong" || len(m.asks) != 1 || m.asks[0] != "Alice|bob|ping|30s" {
		t.Errorf("ask: %+v %v", res, m.asks)
	}
	// A wait longer than a day is cut to a day.
	callMessage(host, map[string]any{"agent": "bob", "message": "ping", "wait_seconds": 1e12})
	if m.asks[1] != "Alice|bob|ping|24h0m0s" {
		t.Errorf("long wait: %v", m.asks[1])
	}

	// A caller that is not a configured agent is named by its id.
	host.AgentID = "temp-1"
	callMessage(host, map[string]any{"agent": "bob", "message": "hi", "wait_seconds": 0.0})
	if m.whispers[1] != "temp-1|temp-1|bob|hi" {
		t.Errorf("unconfigured caller: %v", m.whispers[1])
	}
}

func TestMessageTool_SendFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		wait float64
		want string
	}{
		{errors.New("queue full"), 0, "Could not whisper to Bob: queue full"},
		{errors.New("bus closed"), 5, "Could not ask Bob: bus closed"},
		{tools.ErrMaxDepth, 5, tools.ErrMaxDepth.Error()},
		{tools.ErrAskLoop, 5, tools.ErrAskLoop.Error()},
	} {
		res := callMessage(messageHost(&fakeMessenger{err: tc.err}), map[string]any{"agent": "bob", "message": "hi", "wait_seconds": tc.wait})
		if !res.IsError || res.ForLLM != tc.want {
			t.Errorf("%v: %+v", tc.err, res)
		}
	}
}

// --- the provider's handlers ---

// fakeSpawner is a spawner, inspector and canceller whose answers the test
// sets.
type fakeSpawner struct {
	reqs      []global.SpawnRequest
	statusErr error
	listErr   error
	cancelErr error
	cancelled []string
}

func (f *fakeSpawner) Spawn(_ context.Context, req global.SpawnRequest) (*toolspec.Result, error) {
	f.reqs = append(f.reqs, req)
	return &toolspec.Result{ForLLM: "spawned"}, nil
}

func (f *fakeSpawner) TaskStatus(id string) (*global.TaskStatus, error) {
	return &global.TaskStatus{UUID: id, Status: "running"}, f.statusErr
}

func (f *fakeSpawner) TaskList() ([]global.TaskBrief, error) {
	return []global.TaskBrief{{UUID: "u1", Name: "n1", Status: "done"}}, f.listErr
}

func (f *fakeSpawner) CancelTask(id string) (*global.TaskCancelResult, error) {
	f.cancelled = append(f.cancelled, id)
	return &global.TaskCancelResult{}, f.cancelErr
}

func (f *fakeSpawner) CancelAllTasks() (*global.TaskCancelResult, error) {
	f.cancelled = append(f.cancelled, "*")
	return &global.TaskCancelResult{}, f.cancelErr
}

func providerHandlers(t *testing.T, deps toolspec.Deps) map[string]toolspec.ToolHandler {
	t.Helper()
	out := map[string]toolspec.ToolHandler{}
	for _, d := range GlobalProvider.RegisterTools(deps) {
		out[d.Name] = d.Handler
	}
	return out
}

func run(t *testing.T, h toolspec.ToolHandler, args map[string]any) *toolspec.Result {
	t.Helper()
	res, err := h(&toolspec.ToolCall{Ctx: context.Background(), Args: args})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

func TestGlobalProvider_MetadataAndUnavailable(t *testing.T) {
	if GlobalProvider.Namespace() != "agent" || GlobalProvider.Description() != "Subagent spawning and task tracking" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
	h := providerHandlers(t, toolspec.Deps{})
	if _, ok := h["cancel"]; ok {
		t.Error("agent_cancel listed in the deps-free enumeration")
	}
	for name, want := range map[string]string{
		"spawn":   "spawn tool not available",
		"status":  "task status not available",
		"list":    "task list not available",
		"message": "agent_message is not available",
	} {
		if res := run(t, h[name], map[string]any{"uuid": "u"}); !res.IsError || res.ForLLM != want {
			t.Errorf("%s: %+v", name, res)
		}
	}
	// With real dependencies but no canceller, agent_cancel says so.
	h = providerHandlers(t, toolspec.Deps{Host: tools.ToolDeps{Cfg: &config.Config{}}})
	if res := run(t, h["cancel"], map[string]any{"all": true}); !res.IsError || res.ForLLM != "task cancel not available" {
		t.Errorf("cancel: %+v", res)
	}
}

func TestGlobalProvider_TaskHandlers(t *testing.T) {
	sp := &fakeSpawner{}
	h := providerHandlers(t, toolspec.Deps{Spawn: sp, Host: tools.ToolDeps{Cfg: &config.Config{}}})

	if res := run(t, h["status"], map[string]any{"uuid": " "}); !res.IsError || res.ForLLM != "uuid is required" {
		t.Errorf("status without uuid: %+v", res)
	}
	if res := run(t, h["status"], map[string]any{"uuid": "u1"}); res.IsError || !strings.Contains(res.ForLLM, `"uuid":"u1"`) || !res.Silent {
		t.Errorf("status: %+v", res)
	}
	sp.statusErr = errors.New("disk gone")
	if res := run(t, h["status"], map[string]any{"uuid": "u1"}); !res.IsError || res.ForLLM != "status lookup failed: disk gone" {
		t.Errorf("status failure: %+v", res)
	}
	if res := run(t, h["list"], nil); res.IsError || !strings.Contains(res.ForLLM, `"tasks":[{`) {
		t.Errorf("list: %+v", res)
	}
	sp.listErr = errors.New("disk gone")
	if res := run(t, h["list"], nil); !res.IsError || res.ForLLM != "task list failed: disk gone" {
		t.Errorf("list failure: %+v", res)
	}

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"uuid": "u1", "all": true}, "give either uuid or all, not both"},
		{map[string]any{}, "uuid or all is required"},
		{map[string]any{"all": "yes"}, "uuid or all is required"},
	} {
		if res := run(t, h["cancel"], tc.args); !res.IsError || res.ForLLM != tc.want {
			t.Errorf("cancel %v: %+v", tc.args, res)
		}
	}
	if res := run(t, h["cancel"], map[string]any{"uuid": " u2 "}); res.IsError || !res.Silent {
		t.Errorf("cancel one: %+v", res)
	}
	if res := run(t, h["cancel"], map[string]any{"all": true}); res.IsError {
		t.Errorf("cancel all: %+v", res)
	}
	if len(sp.cancelled) != 2 || sp.cancelled[0] != "u2" || sp.cancelled[1] != "*" {
		t.Errorf("cancelled = %v", sp.cancelled)
	}
	sp.cancelErr = errors.New("busy")
	if res := run(t, h["cancel"], map[string]any{"all": true}); !res.IsError || res.ForLLM != "cancel failed: busy" {
		t.Errorf("cancel failure: %+v", res)
	}
}

// A spawn over the MCP host that cannot be tied to one turn: a background
// task is refused (it would have no chat to report to); a waited one runs
// without a late-result path.
func TestGlobalProvider_SpawnAmbiguousTurn(t *testing.T) {
	sp := &fakeSpawner{}
	h := providerHandlers(t, toolspec.Deps{Spawn: sp})
	ctx := tools.WithReplyChat(context.Background(), tools.ReplyChat{Channel: "telegram", ChatID: "1", Ambiguous: true})
	call := func(args map[string]any) *toolspec.Result {
		res, err := h["spawn"](&toolspec.ToolCall{Ctx: ctx, Args: args, Notify: func(*toolspec.Result) {}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(map[string]any{"task": "t", "name": "n"}); !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || len(sp.reqs) != 0 {
		t.Errorf("callback: %+v", res)
	}
	if res := call(map[string]any{"task": "t", "mode": "wait"}); res.IsError || len(sp.reqs) != 1 || sp.reqs[0].OnResult != nil {
		t.Errorf("wait: %+v %+v", res, sp.reqs)
	}
	if res := call(map[string]any{"task": "t", "media": []any{"file:///etc/passwd"}}); !res.IsError {
		t.Errorf("bad media: %+v", res)
	}
	if res := call(map[string]any{"task": "t", "files": "not a list"}); !res.IsError {
		t.Errorf("bad files: %+v", res)
	}
}
