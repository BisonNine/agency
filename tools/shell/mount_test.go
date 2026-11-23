// ClawEh
// License: MIT

package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sdkshell "go.bison9.com/agent/toolbox/shell"
	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/tools"
)

// agentTools builds the shell tools for an agent on m, as the loop does,
// and registers them on a registry of their own.
type agentTools struct {
	t     *testing.T
	reg   *tools.ToolRegistry
	names []string
}

func build(t *testing.T, m *Mount, cfg *config.Config, deps tools.ToolDeps) *agentTools {
	t.Helper()
	deps.Cfg = cfg
	if deps.AgentID == "" {
		deps.AgentID = deps.AgentCfg.ID
	}
	built := tools.NamespacedProvider("shell", provider{m: m}).Build(deps)
	a := &agentTools{t: t, reg: tools.NewToolRegistry()}
	for _, tool := range built {
		if deps.AgentCfg.IsToolAllowed(tool.Name()) {
			a.reg.Register(tool)
			a.names = append(a.names, tool.Name())
		}
	}
	return a
}

// newMount is a mount of the test's own, closed at its end.
func testMount(t *testing.T) *Mount {
	t.Helper()
	// The test's temporary folders are created (and their removal
	// registered) first, so Close stops the jobs before they are removed.
	t.TempDir()
	m := newMount()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m
}

func shellAgent(id string, deny ...string) *config.AgentConfig {
	return &config.AgentConfig{ID: id, Tools: []string{config.ShellExecTool}, DenyTools: deny}
}

// run calls a tool the way the loop does, with an async callback.
func (a *agentTools) run(name string, args map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.reg.ExecuteWithContext(ctx, name, args, "telegram", "chat-1", cb)
}

var jobIDPattern = regexp.MustCompile(`job (j[0-9a-f]{8})`)

func jobID(t *testing.T, text string) string {
	t.Helper()
	m := jobIDPattern.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no job id in %q", text)
	}
	return m[1]
}

func TestGuardStillRefuses(t *testing.T) {
	m := testMount(t)
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.CustomAllowPatterns = []string{`^git\s+push\s+origin\s+alice\b`}
	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})

	for _, tc := range []struct {
		command string
		refused bool
	}{
		{"rm -rf /tmp/x", true},
		{"kill 1", true},
		{"echo $(whoami)", true},
		{"git push origin main", true},
		{"git push origin alice", false}, // custom allow pattern
		{"echo ok", false},
	} {
		for _, background := range []bool{false, true} {
			res := alice.run("shell_exec", map[string]any{"command": tc.command, "background": background}, nil)
			refused := strings.Contains(res.ForLLM, "Command blocked by safety guard")
			if refused != tc.refused {
				t.Errorf("%q background=%v: %q, refused=%v want %v", tc.command, background, res.ForLLM, refused, tc.refused)
			}
		}
	}
	m.StopAll("alice")

	// With the deny patterns off, nothing is refused.
	cfg = config.DefaultConfig()
	cfg.Tools.Exec.EnableDenyPatterns = false
	bob := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("bob"), Workspace: t.TempDir()})
	if res := bob.run("shell_exec", map[string]any{"command": "echo kill"}, nil); res.IsError {
		t.Errorf("deny patterns off: %q", res.ForLLM)
	}
}

func TestInvalidPatternBuildsNoTools(t *testing.T) {
	m := testMount(t)
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.CustomDenyPatterns = []string{"("}
	if a := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()}); len(a.names) != 0 {
		t.Errorf("tools %v built with an invalid pattern", a.names)
	}
}

func TestChildEnvIsAllowlisted(t *testing.T) {
	t.Setenv("CLAW_GATEWAY_TOKEN", "secret-token")
	t.Setenv("ALERTER_PUSHOVER_TOKEN", "secret-alerter")
	t.Setenv("CLAWEH_TEST_MARKER", "visible-only-if-inherited")
	m := testMount(t)
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})

	res := alice.run("shell_exec", map[string]any{"command": "env"}, nil)
	if res.IsError {
		t.Fatalf("env: %s", res.ForLLM)
	}
	for line := range strings.SplitSeq(res.ForLLM, "\n") {
		if strings.HasPrefix(line, "CLAW_") || strings.HasPrefix(line, "ALERTER_") || strings.HasPrefix(line, "CLAWEH_TEST_MARKER=") {
			t.Errorf("child inherited %q", line)
		}
	}
	if !strings.Contains(res.ForLLM, "PATH=") {
		t.Errorf("child has no PATH:\n%s", res.ForLLM)
	}
}

func TestWorkspaceAndWorkingDir(t *testing.T) {
	m := testMount(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "files", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})

	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if res := alice.run("shell_exec", map[string]any{"command": "pwd -P"}, nil); strings.TrimSpace(res.ForLLM) != realWS {
		t.Errorf("default directory = %q, want the workspace %q", res.ForLLM, realWS)
	}
	res := alice.run("shell_exec", map[string]any{"command": "pwd -P", "working_dir": "files/src"}, nil)
	if strings.TrimSpace(res.ForLLM) != filepath.Join(realWS, "files", "src") {
		t.Errorf("relative working_dir = %q", res.ForLLM)
	}
	// No path confinement: any directory may be named.
	other := t.TempDir()
	res = alice.run("shell_exec", map[string]any{"command": "pwd -P", "working_dir": other}, nil)
	realOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.ForLLM) != realOther {
		t.Errorf("absolute working_dir = %q, want %q", res.ForLLM, realOther)
	}
}

// A clone acts as its source: it sees and stops the source's jobs; another
// agent does not, even in the same workspace.
func TestOwnershipCloneActsAsSource(t *testing.T) {
	m := testMount(t)
	ws := t.TempDir()
	cfg := config.DefaultConfig()
	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	clone := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), AgentID: "alice", Workspace: ws, TempAgent: true, CloneMarker: "1a2b3c4d"})
	bob := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("bob"), Workspace: ws})

	res := clone.run("shell_exec", map[string]any{"command": "sleep 30", "background": true, "notify": false}, nil)
	id := jobID(t, res.ForLLM)

	if res := alice.run("shell_ctrl", map[string]any{"action": "list"}, nil); !strings.Contains(res.ForLLM, id) {
		t.Errorf("alice does not see the job her clone started: %q", res.ForLLM)
	}
	if res := bob.run("shell_ctrl", map[string]any{"action": "list"}, nil); res.ForLLM != "No jobs." {
		t.Errorf("bob's list = %q, want no jobs", res.ForLLM)
	}
	if res := bob.run("shell_ctrl", map[string]any{"action": "stop", "id": id}, nil); !strings.Contains(res.ForLLM, "Unknown job id") {
		t.Errorf("bob stopping alice's job = %q, want unknown", res.ForLLM)
	}
	if m.Running("alice") != 1 || m.Running("bob") != 0 {
		t.Errorf("running: alice %d, bob %d; want 1 and 0", m.Running("alice"), m.Running("bob"))
	}
	if res := alice.run("shell_ctrl", map[string]any{"action": "stop", "id": id}, nil); !strings.Contains(res.ForLLM, "Stopped job "+id) {
		t.Errorf("alice stopping the clone's job = %q", res.ForLLM)
	}
}

// Without shell_ctrl (deny_tools) a foreground command reaching its
// timeout is killed, process group and all, as before; background is
// refused. With it, the command is kept as a job.
func TestTimeoutKeepsJobOnlyWithShellCtrl(t *testing.T) {
	m := testMount(t)
	cfg := config.DefaultConfig()

	ws := t.TempDir()
	bob := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("bob", config.ShellCtrlTool), Workspace: ws})
	if strings.Join(bob.names, ",") != "shell_exec" {
		t.Fatalf("bob's tools = %v, want shell_exec alone", bob.names)
	}
	res := bob.run("shell_exec", map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "timeout_seconds": 1}, nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "timed out after 1s and was stopped") {
		t.Fatalf("timeout without shell_ctrl = %q", res.ForLLM)
	}
	data, err := os.ReadFile(filepath.Join(ws, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(strings.TrimSpace(string(data)), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running after the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m.Running("bob") != 0 {
		t.Errorf("bob runs %d jobs after the timeout", m.Running("bob"))
	}
	res = bob.run("shell_exec", map[string]any{"command": "sleep 1", "background": true}, nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "need shell_ctrl") {
		t.Errorf("background without shell_ctrl = %q", res.ForLLM)
	}

	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})
	res = alice.run("shell_exec", map[string]any{"command": "sleep 30", "timeout_seconds": 1}, nil)
	if res.IsError || !strings.Contains(res.ForLLM, "kept as job") {
		t.Fatalf("timeout with shell_ctrl = %q", res.ForLLM)
	}
	if m.Running("alice") != 1 {
		t.Errorf("alice runs %d jobs, want the kept one", m.Running("alice"))
	}
}

// Classifiers and watch probes run tools unattended: shell_ctrl and
// background are refused there, a foreground command runs.
func TestUnattendedRefusesJobs(t *testing.T) {
	m := testMount(t)
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})
	ctx := context.Background()

	if _, err := alice.reg.ExecuteUnattended(ctx, "shell_ctrl", map[string]any{"action": "list"}); err == nil || !strings.Contains(err.Error(), "not available in unattended runs") {
		t.Errorf("unattended shell_ctrl = %v", err)
	}
	if _, err := alice.reg.ExecuteUnattended(ctx, "shell_exec", map[string]any{"command": "echo hi", "background": true}); err == nil || !strings.Contains(err.Error(), "not available in unattended runs") {
		t.Errorf("unattended background = %v", err)
	}
	out, err := alice.reg.ExecuteUnattended(ctx, "shell_exec", map[string]any{"command": "echo hi"})
	if err != nil || strings.TrimSpace(out) != "hi" {
		t.Errorf("unattended foreground = (%q, %v)", out, err)
	}
}

// The end of a job reaches the agent through the async callback of the
// call that started it: the path a callback sub-agent's result takes.
func TestNoticeThroughTheCallbackPath(t *testing.T) {
	m := testMount(t)
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})

	got := make(chan *toolkit.Result, 1)
	cb := func(_ context.Context, r *toolkit.Result) { got <- r }
	res := alice.run("shell_exec", map[string]any{"command": "sleep 0.3; echo alice-done", "background": true}, cb)
	id := jobID(t, res.ForLLM)
	select {
	case r := <-got:
		if !strings.Contains(r.ForLLM, "Shell job "+id+" ended: exited with code 0") || !strings.Contains(r.ForLLM, "alice-done") {
			t.Errorf("notice = %q", r.ForLLM)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notice")
	}

	// notify false: no notice.
	quiet := make(chan *toolkit.Result, 1)
	res = alice.run("shell_exec", map[string]any{"command": "true", "background": true, "notify": false}, func(_ context.Context, r *toolkit.Result) { quiet <- r })
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	select {
	case r := <-quiet:
		t.Errorf("notice with notify off: %q", r.ForLLM)
	case <-time.After(time.Second):
	}
}

func TestAuditRows(t *testing.T) {
	if err := audit.Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := audit.Close(); err != nil {
			t.Errorf("audit Close: %v", err)
		}
	})
	m := testMount(t)
	m.Bind(Hooks{TurnID: func(context.Context) string { return "turn-7" }})
	ws := t.TempDir()
	clone := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), AgentID: "alice", Workspace: ws, CloneMarker: "1a2b3c4d"})

	res := clone.run("shell_exec", map[string]any{"command": "cat", "background": true, "notify": false}, nil)
	id := jobID(t, res.ForLLM)
	if res := clone.run("shell_ctrl", map[string]any{"action": "input", "id": id, "text": "secret words"}, nil); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if res := clone.run("shell_ctrl", map[string]any{"action": "stop", "id": id}, nil); res.IsError {
		t.Fatal(res.ForLLM)
	}
	store := audit.Default()
	ctx := context.Background()
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Query(ctx, audit.Filter{Agent: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]audit.Event{}
	for _, r := range rows {
		byKind[r.Kind] = r
		if strings.Contains(r.Details, "secret words") {
			t.Errorf("%s row holds the input text: %s", r.Kind, r.Details)
		}
	}
	for kind, want := range map[string]map[string]any{
		audit.KindShellStart: {"job": id, "command": "cat", "background": true, "clone": "1a2b3c4d", "chat_id": "chat-1"},
		audit.KindShellInput: {"job": id, "bytes": float64(len("secret words\n"))},
		audit.KindShellStop:  {"job": id},
	} {
		r, ok := byKind[kind]
		if !ok {
			t.Errorf("no %s row in %+v", kind, rows)
			continue
		}
		if r.TurnID != "turn-7" || r.Channel != "telegram" {
			t.Errorf("%s: turn %q channel %q", kind, r.TurnID, r.Channel)
		}
		var details map[string]any
		if err := json.Unmarshal([]byte(r.Details), &details); err != nil {
			t.Fatalf("%s details: %v", kind, err)
		}
		for k, v := range want {
			if details[k] != v {
				t.Errorf("%s details[%s] = %v, want %v", kind, k, details[k], v)
			}
		}
	}
	if byKind[audit.KindShellStart].Tool != "shell_exec" || byKind[audit.KindShellInput].Tool != "shell_ctrl" {
		t.Errorf("tools: start %q, input %q", byKind[audit.KindShellStart].Tool, byKind[audit.KindShellInput].Tool)
	}
}

// The jobs folder and its files take the agents area's modes.
func TestModesOnDisk(t *testing.T) {
	m := testMount(t)
	ws := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.List = []config.AgentConfig{{ID: "alice", Workspace: ws}}
	cfg.Security.Permissions.Agents = config.AreaPermissions{DirMode: "0750", FileMode: "0640"}
	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})

	res := alice.run("shell_exec", map[string]any{"command": "echo hi", "background": true, "notify": false}, nil)
	id := jobID(t, res.ForLLM)
	if res := alice.run("shell_ctrl", map[string]any{"action": "wait", "id": id}, nil); !strings.Contains(res.ForLLM, "exited with code 0") {
		t.Fatalf("wait = %q", res.ForLLM)
	}
	dir := filepath.Join(ws, config.ShellJobsDir)
	checkMode(t, dir, 0o750)
	checkMode(t, filepath.Join(dir, id+".log"), 0o640)
	checkMode(t, filepath.Join(dir, id+".json"), 0o640)
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s: mode %o, want %o", path, got, want)
	}
}

// Close stops every job before it returns.
func TestCloseStopsJobs(t *testing.T) {
	m := newMount()
	ws := t.TempDir()
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	res := alice.run("shell_exec", map[string]any{"command": "sleep 60 & echo $! > child.pid; wait", "background": true}, nil)
	jobID(t, res.ForLLM)
	var pid int
	waitFor(t, func() bool {
		data, err := os.ReadFile(filepath.Join(ws, "child.pid"))
		if err != nil {
			return false
		}
		_, err = fmt.Sscan(strings.TrimSpace(string(data)), &pid)
		return err == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, func() bool { return !processAlive(pid) })
	if m.Running("alice") != 0 {
		t.Errorf("alice runs %d jobs after Close", m.Running("alice"))
	}
}

// A job found running when the service starts again was lost: it is
// marked so, the agent is told once (through the loop's hooks, once
// bound), and it is not started again.
func TestLostAfterRestart(t *testing.T) {
	ws := t.TempDir()
	first := newMount()
	alice := build(t, first, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	res := alice.run("shell_exec", map[string]any{"command": "sleep 60", "background": true}, nil)
	id := jobID(t, res.ForLLM)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := first.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// A second process over the same workspace: the record says running.
	second := testMount(t)
	again := build(t, second, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})

	told := bindRecorder(t, second, 1)
	if len(told) != 1 || !strings.HasPrefix(told[0], "alice: Shell job "+id+" was lost") {
		t.Errorf("told = %q, want one lost notice for alice", told)
	}

	res = again.run("shell_ctrl", map[string]any{"action": "list"}, nil)
	if !strings.Contains(res.ForLLM, id+" lost") {
		t.Errorf("list after restart = %q", res.ForLLM)
	}
	if second.Running("alice") != 0 {
		t.Errorf("the lost job was started again")
	}
	var rec struct {
		State proc.State `json:"state"`
	}
	data, err := os.ReadFile(filepath.Join(ws, config.ShellJobsDir, id+".json"))
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil || rec.State != proc.StateLost {
		t.Errorf("record state = %q (%v), want lost", rec.State, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Jobs stopped by Close send no notice: the loop is stopping.
func TestCloseSendsNoNotice(t *testing.T) {
	m := newMount()
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})
	got := make(chan *toolkit.Result, 1)
	res := alice.run("shell_exec", map[string]any{"command": "sleep 60", "background": true}, func(_ context.Context, r *toolkit.Result) { got <- r })
	jobID(t, res.ForLLM)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case r := <-got:
		t.Errorf("notice at shutdown: %q", r.ForLLM)
	case <-time.After(500 * time.Millisecond):
	}
}

// The notice of a background job that exits at once still reaches the
// callback of the call that started it.
func TestNoticeOfAJobThatExitsAtOnce(t *testing.T) {
	m := testMount(t)
	alice := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: t.TempDir()})
	for range 20 {
		got := make(chan *toolkit.Result, 1)
		res := alice.run("shell_exec", map[string]any{"command": "true", "background": true}, func(_ context.Context, r *toolkit.Result) { got <- r })
		id := jobID(t, res.ForLLM)
		select {
		case r := <-got:
			if !strings.Contains(r.ForLLM, "Shell job "+id+" ended") {
				t.Errorf("notice = %q", r.ForLLM)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no notice for job %s", id)
		}
	}
}

// A job stopped by a clean shutdown is announced once at the next start,
// to the owner's main conversation.
func TestShutdownStoppedJobToldAtNextStart(t *testing.T) {
	ws := t.TempDir()
	first := newMount()
	alice := build(t, first, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	id := jobID(t, alice.run("shell_exec", map[string]any{"command": "sleep 60", "background": true}, func(context.Context, *toolkit.Result) {}).ForLLM)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second := testMount(t)
	build(t, second, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	told := bindRecorder(t, second, 1)
	want := "alice: Shell job " + id + " was stopped when the service restarted"
	if len(told) != 1 || !strings.HasPrefix(told[0], want) {
		t.Errorf("told = %q, want one notice starting %q", told, want)
	}
}

// A reload applies tools.exec's timeout to the next command of an agent
// whose tools already exist.
func TestReloadUpdatesTimeout(t *testing.T) {
	m := testMount(t)
	ws := t.TempDir()
	cfg := config.DefaultConfig()
	alice := build(t, m, cfg, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	if res := alice.run("shell_exec", map[string]any{"command": "sleep 2; echo slept"}, nil); !strings.Contains(res.ForLLM, "slept") {
		t.Fatalf("before the reload = %q", res.ForLLM)
	}
	next := config.DefaultConfig()
	next.Tools.Exec.TimeoutSeconds = 1
	alice = build(t, m, next, tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	if res := alice.run("shell_exec", map[string]any{"command": "sleep 3; echo slept"}, nil); !strings.Contains(res.ForLLM, "kept as job") {
		t.Errorf("after the reload = %q, want kept at the 1s timeout", res.ForLLM)
	}
}

// bindRecorder binds hooks that record the delivered notices, waits for n
// of them, and returns what was delivered a moment later (so a duplicate
// would show).
func bindRecorder(t *testing.T, m *Mount, n int) []string {
	t.Helper()
	var mu sync.Mutex
	var told []string
	m.Bind(Hooks{Deliver: func(owner, text string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, owner+": "+text)
	}})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(told) >= n
	})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), told...)
}

// After Close no tools are built: nothing can start a job or re-read the
// stopped jobs' records before the process exits.
func TestClosedMountBuildsNothing(t *testing.T) {
	m := newMount()
	ws := t.TempDir()
	build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if a := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("alice"), Workspace: ws}); len(a.names) != 0 {
		t.Errorf("tools %v built after Close", a.names)
	}
	if a := build(t, m, config.DefaultConfig(), tools.ToolDeps{AgentCfg: shellAgent("bob"), Workspace: t.TempDir()}); len(a.names) != 0 {
		t.Errorf("tools %v built for a new workspace after Close", a.names)
	}
}

// Notices kept before the hooks are bound wait for a Deliver hook, and are
// then delivered in the order they came.
func TestPendingNoticesKeptAndOrdered(t *testing.T) {
	m := testMount(t)
	for _, text := range []string{"Shell job j1 was lost", "Shell job j2 was lost", "Shell job j3 was lost"} {
		m.notice(sdkshellNotice("alice", text))
	}
	m.Bind(Hooks{TurnID: func(context.Context) string { return "" }})
	if m.Pending() != 3 {
		t.Fatalf("pending = %d after binding without Deliver, want 3", m.Pending())
	}
	told := bindRecorder(t, m, 3)
	want := []string{"alice: Shell job j1 was lost", "alice: Shell job j2 was lost", "alice: Shell job j3 was lost"}
	if strings.Join(told, "|") != strings.Join(want, "|") {
		t.Errorf("told = %q, want %q", told, want)
	}
}

func sdkshellNotice(owner, text string) sdkshell.Notice {
	return sdkshell.Notice{Owner: owner, JobID: "j1", State: proc.StateLost, Text: text}
}
