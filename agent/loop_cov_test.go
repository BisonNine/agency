// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/cogmem/consolidate"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/alerts"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/mcp"
	"go.bison9.com/agency/msgtoken"
	"go.bison9.com/agency/state"
)

// The registry's clock is the loop's: every call reads the clock the loop has
// at that moment, so the registry's timers move with a fake put on the loop.
func TestLoopCov_RegistryClockFollowsLoopClock(t *testing.T) {
	tl := newTestAgentLoop(t)
	start := time.Date(2026, time.November, 14, 9, 0, 0, 0, time.UTC)
	fc := clock.NewFake(start)
	tl.al.clock = fc
	rc := registryClock{tl.al}

	if got := rc.Until(start.Add(time.Hour)); got != time.Hour {
		t.Fatalf("Until = %v, want 1h", got)
	}

	after := rc.After(time.Minute)
	fired := make(chan struct{})
	timer := rc.AfterFunc(2*time.Minute, func() { close(fired) })
	defer timer.Stop()
	timeoutCtx, cancelTimeout := rc.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelTimeout()
	deadlineCtx, cancelDeadline := rc.WithDeadline(context.Background(), start.Add(4*time.Minute))
	defer cancelDeadline()
	if d, ok := deadlineCtx.Deadline(); !ok || !d.Equal(start.Add(4*time.Minute)) {
		t.Fatalf("WithDeadline deadline = %v (%v), want %v", d, ok, start.Add(4*time.Minute))
	}

	slept := make(chan struct{})
	go func() {
		rc.Sleep(5 * time.Minute)
		close(slept)
	}()
	// After, AfterFunc, the two contexts and the sleep all wait on the fake.
	advanceWhenWaiting(t, fc, 5, 5*time.Minute)

	select {
	case <-after:
	default:
		t.Fatal("After did not fire on the loop's clock")
	}
	awaitSignal(t, fired, "AfterFunc on the loop's clock")
	awaitSignal(t, timeoutCtx.Done(), "WithTimeout on the loop's clock")
	awaitSignal(t, deadlineCtx.Done(), "WithDeadline on the loop's clock")
	awaitSignal(t, slept, "Sleep on the loop's clock")
}

// loopCovAlerts records the alerts sent to it on a channel.
type loopCovAlerts struct{ ch chan alerter.Alert }

func newLoopCovAlerts() *loopCovAlerts { return &loopCovAlerts{ch: make(chan alerter.Alert, 16)} }

func (a *loopCovAlerts) Send(al alerter.Alert)               { a.ch <- al }
func (a *loopCovAlerts) Normal(string, string, ...string)    {}
func (a *loopCovAlerts) Urgent(string, string, ...string)    {}
func (a *loopCovAlerts) Emergency(string, string, ...string) {}
func (a *loopCovAlerts) Close(context.Context) error         { return nil }

// next returns the next alert, failing the test when none comes.
func (a *loopCovAlerts) next(t *testing.T) alerter.Alert {
	t.Helper()
	select {
	case got := <-a.ch:
		return got
	case <-time.After(30 * time.Second):
		t.Fatal("no alert was sent")
		return alerter.Alert{}
	}
}

// An unreadable named-token store does not stop the loop: it starts with an
// empty in-memory store, alerts the operator once, and leaves the file as it
// is until the next change.
func TestLoopCov_NewAgentLoopUnreadableNamedTokens(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	rec := newLoopCovAlerts()
	alerts.Set(rec)
	t.Cleanup(func() { alerts.Set(nil) })

	cfg := ownerTestConfig(t)
	path := msgtoken.NamedTokenPath(cfg.DataDir())
	if err := os.MkdirAll(path, 0o700); err != nil { // a directory cannot be read as the file
		t.Fatal(err)
	}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)

	got := rec.next(t)
	if got.EventID != "msgtoken:named" || got.Title != "Named message-token store unreadable" || !strings.HasPrefix(got.Description, path+":") {
		t.Fatalf("alert = %+v, want the named-token store alert for %s", got, path)
	}
	tok, err := al.CreateMessageToken("main", "alice")
	if err != nil {
		t.Fatalf("CreateMessageToken on the fallback store: %v", err)
	}
	if id, ok := al.ValidateMessageToken(tok.Token); !ok || id != "main" {
		t.Fatalf("token from the fallback store resolves to %q (%v), want main", id, ok)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("the unreadable store was replaced: %v", err)
	}
}

// An agent that cannot be built fails the loop's construction with the
// agent named.
func TestLoopCov_NewAgentLoopAgentBuildFails(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.BaseDir = filepath.Join(blocker, "agents") // under a file: no workspace can be made

	al, err := NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err == nil {
		al.GetRegistry().Close()
		t.Fatal("NewAgentLoop succeeded with an unbuildable agent")
	}
	if !strings.Contains(err.Error(), `agent "main"`) {
		t.Fatalf("error %q does not name the agent", err)
	}
}

// Run serves the inbound bus: it closes Started, replays the turn a restart
// interrupted (with the restart notice), answers it, and returns nil once
// Stop is called. WaitTurns then returns and Close may be called twice.
func TestLoopCov_RunRecoversAndServesUntilStop(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	al := tl.al
	agent := mustGetAgent(t, al)

	const key = "agent:main:main"
	addFullMessage(t, agent.Sessions, key, spawnllm.Message{Role: "user", Content: "what is 2+2"})
	if err := agent.Sessions.SetPendingTurn(key); err != nil {
		t.Fatal(err)
	}
	if err := testStateManager(t, al, "main").SetPendingTurn(key, state.PendingTurn{Channel: "webui", ChatID: "c1"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-al.Started():
		t.Fatal("Started closed before Run")
	default:
	}

	runErr := make(chan error, 1)
	go func() { runErr <- al.Run(t.Context()) }()
	awaitSignal(t, al.Started(), "Run to start")

	var notice, reply bool
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for !notice || !reply {
		out, ok := tl.msgBus.SubscribeOutbound(ctx)
		if !ok {
			t.Fatalf("outbound ended before the notice (%v) and the reply (%v)", notice, reply)
		}
		if out.Channel != "webui" || out.ChatID != "c1" {
			continue
		}
		switch out.Content {
		case recoveryReplayNotice:
			notice = true
		case "Mock response":
			reply = true
		}
	}

	al.Stop()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil after Stop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if err := al.WaitTurns(ctx); err != nil {
		t.Fatalf("WaitTurns: %v", err)
	}
	al.Close(context.Background())
	al.Close(context.Background()) // a second Close finds everything closed
}

// Run returns nil when the context it serves under ends.
func TestLoopCov_RunEndsWithContext(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	tl := newTestAgentLoop(t)
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- tl.al.Run(ctx) }()
	awaitSignal(t, tl.al.Started(), "Run to start")
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	tl.al.Close(context.Background())
}

// WaitTurns gives up with ctx's error while a turn is still running.
func TestLoopCov_WaitTurnsHonoursContext(t *testing.T) {
	tl := newTestAgentLoop(t)
	tl.al.activeRequests.Add(1)
	defer tl.al.activeRequests.Done()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := tl.al.WaitTurns(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitTurns = %v, want context.Canceled", err)
	}
}

// BeginMCPShutdown is a no-op without an MCP manager and starts the
// manager's shutdown when there is one; Close then closes it.
func TestLoopCov_BeginMCPShutdown(t *testing.T) {
	tl := newTestAgentLoop(t)
	tl.al.BeginMCPShutdown() // no manager: nothing to do

	mgr := mcp.NewManager()
	tl.al.mcp.setManager(mgr)
	tl.al.BeginMCPShutdown()
	tl.al.BeginMCPShutdown() // repeatable
	if got := mgr.RetryDisconnected(t.Context()); got != nil {
		t.Fatalf("RetryDisconnected after shutdown began = %v, want nothing", got)
	}
	tl.al.Close(context.Background())
	if tl.al.mcp.peekManager() != nil {
		t.Fatal("Close left the MCP manager in place")
	}
}

// SetCogmemManager installs the consolidation manager; nil removes it.
func TestLoopCov_SetCogmemManager(t *testing.T) {
	tl := newTestAgentLoop(t)
	current := func() *consolidate.Manager {
		tl.al.mu.RLock()
		defer tl.al.mu.RUnlock()
		return tl.al.cogmemManager
	}
	mgr := consolidate.NewManager(nil)
	tl.al.SetCogmemManager(mgr)
	if current() != mgr {
		t.Fatal("SetCogmemManager did not install the manager")
	}
	tl.al.SetCogmemManager(nil)
	if current() != nil {
		t.Fatal("SetCogmemManager(nil) kept the manager")
	}
}

// loopCovLateCancel is a context whose Err reports cancellation only from
// the n-th call on, so a test can end it between two checks of one call.
type loopCovLateCancel struct {
	context.Context
	calls int
	n     int
}

func (c *loopCovLateCancel) Err() error {
	c.calls++
	if c.calls >= c.n {
		return context.Canceled
	}
	return nil
}

// A reload whose context ends while it waits for another reload to finish
// changes nothing.
func TestLoopCov_ReloadConfigCanceledWhileWaiting(t *testing.T) {
	tl := newTestAgentLoop(t)
	next := *tl.cfg
	ctx := &loopCovLateCancel{Context: context.Background(), n: 2}

	err := tl.al.ReloadConfig(ctx, &next)
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "before registry creation") {
		t.Fatalf("ReloadConfig = %v, want cancelled before registry creation", err)
	}
	if tl.al.GetConfig() != tl.cfg {
		t.Fatal("a cancelled reload swapped the config")
	}
}

// A reload stops the message-token managers it replaces; the rebuilt one
// for a still-enabled agent keeps validating its token.
func TestLoopCov_ReloadReplacesMessageManagers(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	cfg.Agents.List[0].Message = &config.MessageConfig{WindowMinutes: 60, WindowCount: 2}
	provider := &mockProvider{}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), provider, nil)
	old := al.messageManagers["main"]
	if old == nil {
		t.Fatal("no message-token manager for main")
	}
	tok := old.CurrentToken()

	next := *cfg
	if err := reloadForTest(t, al, context.Background(), provider, &next); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if al.messageManagers["main"] == old {
		t.Fatal("the reload kept the old manager")
	}
	if id, ok := al.ValidateMessageToken(tok); !ok || id != "main" {
		t.Fatalf("token after reload resolves to %q (%v), want main", id, ok)
	}
	al.Close(context.Background())
}

// With no agent configured the startup info is empty.
func TestLoopCov_GetStartupInfoNoAgent(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	cfg := newTestConfig(t)
	cfg.Agents.List = nil
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &mockProvider{}, nil)
	if al.GetRegistry().Default() != nil {
		t.Fatal("the registry has a default agent with none configured")
	}
	if info := al.GetStartupInfo(); len(info) != 0 {
		t.Fatalf("GetStartupInfo = %v, want empty", info)
	}
}
