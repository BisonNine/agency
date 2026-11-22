// ClawEh
// License: MIT

// Package shell mounts the SDK's shell tools (go.bison9.com/agent/toolbox/shell)
// as shell_exec and shell_ctrl. The SDK runs the commands and the jobs; this
// package supplies what depends on the host: the command guard (tools.exec's
// patterns), the allowlisted child environment, the workspace, who owns a
// job (a clone acts as its source), whether the caller has shell_ctrl, the
// unattended rule, the end-of-job notice, the audit rows, the logger, the
// file modes and the limits.
//
// Each workspace has one job manager, kept for the life of the process (a
// reload rebuilds the tools over the same manager, so running jobs carry
// on), with its logs and records in <workspace>/state/shell.
package shell

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/fsmode"
	sdkshell "go.bison9.com/agent/toolbox/shell"
	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/internal/childenv"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// Hooks are what the host's agent loop gives the mount once it runs.
type Hooks struct {
	// TurnID returns the id of the turn a call belongs to, "" when none.
	TurnID func(ctx context.Context) string
	// Deliver tells owner's main conversation text, for a notice no call
	// can carry: a job lost when the service restarted.
	Deliver func(owner, text string)
}

// maxPending bounds the notices kept until the hooks are bound.
const maxPending = 256

// Mount holds the job managers of every workspace and routes their notices.
type Mount struct {
	mu      sync.Mutex
	byDir   map[string]*sdkshell.Tools // per absolute jobs directory
	closing bool                       // Close is stopping the jobs: build nothing

	guard atomic.Pointer[guard]

	// nmu guards the routing state below; never held while calling out.
	nmu     sync.Mutex
	routes  map[string]func(*toolspec.Result) // jobKey → the starting call's async callback
	hooks   Hooks
	bound   bool
	pending []pendingNotice
}

type pendingNotice struct{ owner, text string }

// Host is the process's mount, used by GlobalProvider, the gateway (Bind,
// Close) and the agent loop.
var Host = newMount()

func newMount() *Mount {
	return &Mount{byDir: make(map[string]*sdkshell.Tools), routes: make(map[string]func(*toolspec.Result))}
}

// GlobalProvider publishes the shell tools: the aggregator mounts it under
// the "shell" namespace, so they are shell_exec and shell_ctrl.
var GlobalProvider = provider{m: Host}

type provider struct{ m *Mount }

// Namespace, Description and Available satisfy global.HostMeta.
func (provider) Namespace() string            { return sdkshell.Namespace }
func (provider) Description() string          { return "Shell commands and background jobs" }
func (provider) Available(any) (bool, string) { return true, "" }
func (p provider) RegisterTools(deps global.Deps) []global.ToolDefinition {
	return p.m.definitions(deps)
}

// callInfo is what a call's handler knows about the agent it runs for.
type callInfo struct {
	tool  string              // published name
	owner string              // the agent the jobs belong to (a clone's source)
	agent *config.AgentConfig // the agent's config at this build
	clone string              // a clone's short id, for the audit rows
}

type callInfoKey struct{}

func infoOf(call *toolspec.ToolCall) (callInfo, bool) {
	if call == nil || call.Ctx == nil {
		return callInfo{}, false
	}
	info, ok := call.Ctx.Value(callInfoKey{}).(callInfo)
	return info, ok
}

// definitions builds the tools for one agent. Without real dependencies (the
// catalogue's enumeration) it returns shell_exec alone: shell_ctrl is never a
// tool of its own in the tools list, it comes with shell_exec.
func (m *Mount) definitions(deps global.Deps) []global.ToolDefinition {
	td, ok := deps.Host.(tools.ToolDeps)
	if !ok || td.Cfg == nil {
		for _, d := range new(sdkshell.Tools).Definitions() {
			if sdkshell.Namespace+"_"+d.Name == config.ShellExecTool {
				return []global.ToolDefinition{d}
			}
		}
		return nil
	}
	if !td.AgentCfg.IsToolAllowed(config.ShellExecTool) {
		return nil
	}
	exec := td.Cfg.Tools.Exec
	g, err := newGuard(exec)
	if err != nil {
		logger.ErrorCF("shell", "Shell tools not built: invalid tools.exec pattern",
			map[string]any{"agent_id": td.AgentID, "error": err.Error()})
		return nil
	}
	logGuardState(exec)
	m.guard.Store(g)
	t, err := m.toolsFor(td.Cfg, td.Workspace)
	if err != nil {
		logger.ErrorCF("shell", "Shell tools not built",
			map[string]any{"agent_id": td.AgentID, "workspace": td.Workspace, "error": err.Error()})
		return nil
	}
	defs := t.Definitions()
	for i := range defs {
		d := &defs[i]
		info := callInfo{tool: sdkshell.Namespace + "_" + d.Name, owner: td.AgentID, agent: td.AgentCfg, clone: td.CloneMarker}
		handler := d.Handler
		d.Handler = func(call *toolspec.ToolCall) (*toolspec.Result, error) {
			call.Ctx = context.WithValue(callContext(call), callInfoKey{}, info)
			return handler(call)
		}
		// The bridge hands an async tool the call's callback (ToolCall.Notify):
		// the path a callback sub-agent's result takes, used for the notice.
		d.Async = true
	}
	return defs
}

// toolsFor returns the job manager and tools of workspace, creating them
// the first time with cfg's limits. Jobs found running in its folder were
// lost with an earlier process: their notices go out here.
func (m *Mount) toolsFor(cfg *config.Config, workspace string) (*sdkshell.Tools, error) {
	if workspace == "" {
		return nil, errors.New("no workspace")
	}
	dir, err := filepath.Abs(filepath.Join(workspace, config.ShellJobsDir))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil, errors.New("shutting down")
	}
	if t := m.byDir[dir]; t != nil {
		return t, nil
	}
	ex := cfg.Tools.Exec.Effective()
	modes := cfg.AgentModes(dir)
	t, err := sdkshell.New(sdkshell.Config{
		Jobs: proc.Options{
			Dir: dir,
			Limits: proc.Limits{
				MaxJobs:        ex.MaxJobs,
				MaxJobTime:     time.Duration(ex.MaxJobSeconds) * time.Second,
				MaxOutputBytes: int64(ex.MaxJobOutputBytes),
				LogRetention:   time.Duration(ex.JobLogDays) * 24 * time.Hour,
			},
			Modes: fsmode.Modes{Dir: modes.Dir, File: modes.File},
			OnEnd: func(info proc.Info) { m.forget(dir, info.ID) },
		},
		Timeout:       time.Duration(ex.TimeoutSeconds) * time.Second,
		MaxTimeout:    time.Duration(ex.MaxTimeoutSeconds) * time.Second,
		Guard:         m.check,
		Env:           func(*toolspec.ToolCall) []string { return childenv.Base() },
		Workspace:     func(*toolspec.ToolCall) string { return workspace },
		Owner:         owner,
		CtrlAvailable: ctrlAvailable,
		Unattended:    func(call *toolspec.ToolCall) bool { return tools.IsUnattended(callContext(call)) },
		Notice:        func(n sdkshell.Notice) { m.notice(dir, n) },
		Audit:         func(e sdkshell.AuditEvent) { m.audit(dir, e) },
		Logger:        logger.SDK(),
	})
	if err != nil {
		return nil, err
	}
	m.byDir[dir] = t
	return t, nil
}

// check is the command guard of the current tools.exec settings.
func (m *Mount) check(command string) string {
	g := m.guard.Load()
	if g == nil {
		return ""
	}
	return g.check(command)
}

// owner is the agent a call's jobs belong to: the agent the tools were built
// for, which for a clone is its source.
func owner(call *toolspec.ToolCall) string {
	if info, ok := infoOf(call); ok {
		return info.owner
	}
	return call.AgentID
}

// ctrlAvailable reports whether the calling agent has shell_ctrl: without it
// (deny_tools) a foreground command is stopped at its timeout, as before.
func ctrlAvailable(call *toolspec.ToolCall) bool {
	info, ok := infoOf(call)
	return ok && info.agent.IsToolAllowed(config.ShellCtrlTool)
}

func callContext(call *toolspec.ToolCall) context.Context {
	if call.Ctx != nil {
		return call.Ctx
	}
	return context.Background()
}

func jobKey(dir, id string) string { return dir + "\x00" + id }

// forget drops a job's route once it has ended (after its notice).
func (m *Mount) forget(dir, id string) {
	m.nmu.Lock()
	delete(m.routes, jobKey(dir, id))
	m.nmu.Unlock()
}

// notice delivers an end-of-job notice: through the async callback of the
// call that started the job, as a callback sub-agent's result is delivered,
// so it follows the same turn-chat rules. A job lost in a restart has no
// call: its notice goes to the owner's main conversation through the hooks.
func (m *Mount) notice(dir string, n sdkshell.Notice) {
	if n.State == proc.StateStopped && n.StopReason == proc.StopShutdown {
		// The loop is stopping: a turn started now would not be answered.
		m.forget(dir, n.JobID)
		logger.InfoCF("shell", "Shell job stopped at shutdown; no notice sent",
			map[string]any{"agent_id": n.Owner, "job": n.JobID})
		return
	}
	m.nmu.Lock()
	route := m.routes[jobKey(dir, n.JobID)]
	delete(m.routes, jobKey(dir, n.JobID))
	var deliver func(owner, text string)
	if route == nil && n.State == proc.StateLost {
		if m.bound {
			deliver = m.hooks.Deliver
		} else if len(m.pending) < maxPending {
			m.pending = append(m.pending, pendingNotice{owner: n.Owner, text: n.Text})
		} else {
			logger.WarnCF("shell", "Lost shell job notice not kept: too many waiting",
				map[string]any{"agent_id": n.Owner, "job": n.JobID})
		}
	}
	m.nmu.Unlock()
	switch {
	case route != nil:
		go route(&toolspec.Result{ForLLM: n.Text})
	case deliver != nil:
		go deliver(n.Owner, n.Text)
	case n.State != proc.StateLost:
		logger.InfoCF("shell", "Shell job notice not delivered: no conversation to tell",
			map[string]any{"agent_id": n.Owner, "job": n.JobID})
	}
}

// audit records a start, an input or a stop, and keeps the starting call's
// async callback for the job's notice.
func (m *Mount) audit(dir string, e sdkshell.AuditEvent) {
	ctx := context.Background()
	if e.Call != nil {
		ctx = callContext(e.Call)
	}
	routed := e.Kind == sdkshell.AuditStart && e.JobID != "" && e.Call != nil && e.Call.Notify != nil
	m.nmu.Lock()
	if routed {
		m.routes[jobKey(dir, e.JobID)] = e.Call.Notify
	}
	turnID := m.hooks.TurnID
	m.nmu.Unlock()
	if routed && !m.running(dir, e.JobID) {
		// It ended before its start was recorded: its end has been handled.
		m.forget(dir, e.JobID)
	}

	store := audit.Default()
	if store == nil {
		return
	}
	info, _ := infoOf(e.Call)
	details := map[string]any{"job": e.JobID, "command": e.Command}
	var kind string
	switch e.Kind {
	case sdkshell.AuditStart:
		kind = audit.KindShellStart
		details["dir"] = e.Dir
		details["background"] = e.Background
	case sdkshell.AuditInput:
		kind = audit.KindShellInput
		details["bytes"] = e.Bytes
	case sdkshell.AuditStop:
		kind = audit.KindShellStop
	default:
		return
	}
	ev := audit.Event{Kind: kind, Agent: e.Owner, Tool: info.tool, Outcome: audit.OutcomeOK}
	if e.Call != nil {
		ev.Session, ev.Channel = e.Call.Session, e.Call.Channel
		details["chat_id"] = e.Call.ChatID
	}
	if info.clone != "" {
		details["clone"] = info.clone
	}
	if turnID != nil {
		ev.TurnID = turnID(ctx)
	}
	if b, err := json.Marshal(details); err == nil {
		ev.Details = string(b)
	}
	store.Record(ev)
}

// running reports whether job id of the manager over dir is still running.
func (m *Mount) running(dir, id string) bool {
	m.mu.Lock()
	t := m.byDir[dir]
	m.mu.Unlock()
	if t == nil {
		return false
	}
	info, ok := t.Manager().Get(id)
	return ok && info.Running()
}

// Bind installs the loop's hooks and delivers the notices kept until now.
func (m *Mount) Bind(h Hooks) {
	m.nmu.Lock()
	m.hooks, m.bound = h, h.Deliver != nil
	pending := m.pending
	m.pending = nil
	m.nmu.Unlock()
	if h.Deliver == nil {
		return
	}
	for _, p := range pending {
		h.Deliver(p.owner, p.text)
	}
}

func (m *Mount) all() []*sdkshell.Tools {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*sdkshell.Tools, 0, len(m.byDir))
	for _, t := range m.byDir {
		out = append(out, t)
	}
	return out
}

// Running returns how many jobs owner (an agent id) is running.
func (m *Mount) Running(owner string) int {
	n := 0
	for _, t := range m.all() {
		n += len(t.Running(owner))
	}
	return n
}

// StopAll stops every running job of owner (terminate, grace, kill) and
// returns how many it stopped. Their notices go out as usual.
func (m *Mount) StopAll(owner string) int {
	var wg sync.WaitGroup
	var n atomic.Int64
	for _, t := range m.all() {
		wg.Go(func() { n.Add(int64(t.StopAll(owner))) })
	}
	wg.Wait()
	return int(n.Load())
}

// Close stops every job of every workspace (terminate, grace, kill), with
// no end-of-job notice (the loop is stopping), builds no tools meanwhile, and
// then forgets the managers and the hooks, so a gateway started again in this
// process starts afresh. It returns when the jobs have ended or ctx ends.
func (m *Mount) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	all := make([]*sdkshell.Tools, 0, len(m.byDir))
	for _, t := range m.byDir {
		all = append(all, t)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	errs := make([]error, len(all))
	for i, t := range all {
		wg.Go(func() { errs[i] = t.Close(ctx) })
	}
	wg.Wait()

	m.mu.Lock()
	m.byDir = make(map[string]*sdkshell.Tools)
	m.closing = false
	m.mu.Unlock()
	m.nmu.Lock()
	m.hooks, m.bound = Hooks{}, false
	m.routes = make(map[string]func(*toolspec.Result))
	m.nmu.Unlock()
	return errors.Join(errs...)
}
