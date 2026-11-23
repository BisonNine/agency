// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// probeRun is a hook.Run that records how it was stopped.
type probeRun struct {
	done     chan struct{}
	onCancel func(cause error)
}

func (r *probeRun) Events() iter.Seq2[hook.Event, error] {
	return func(func(hook.Event, error) bool) {}
}
func (r *probeRun) Interrupt()            { r.Cancel(hook.ErrInterrupted) }
func (r *probeRun) Cancel(cause error)    { r.onCancel(cause) }
func (r *probeRun) Done() <-chan struct{} { return r.done }
func (r *probeRun) Wait() hook.TurnResult { <-r.done; return hook.TurnResult{} }

// A /cancel sets the turn's own cause before anything else of the cancel
// runs: when the stopped run ends at once, its outcome is decided with the
// cause in place (never as an error reply). The run is then stopped with
// the same cause.
func TestSessionCancel_CauseSetBeforeTheEngineStops(t *testing.T) {
	turnCtx, stopTurn := context.WithCancelCause(context.Background())
	var causeAtStop, causeGiven error
	run := &probeRun{done: make(chan struct{})}
	run.onCancel = func(c error) {
		causeAtStop, causeGiven = context.Cause(turnCtx), c
		close(run.done) // as if the run finished at once
	}
	al := &AgentLoop{cfg: &config.Config{}}
	alice := &AgentInstance{ID: "alice"}
	_, ht := al.withHostTurn(context.Background(), alice, processOptions{SessionKey: "s"}, nil, nil, nil)
	h := &runHandle{al: al, agent: alice, run: run, turn: ht}
	if !h.running() {
		t.Fatal("a run not yet finished does not read as running")
	}
	hs := &turnHandles{h: h, claimed: true}
	ss := &sessionState{turnCancel: stopTurn, handles: hs}

	ss.cancel(&userCancelError{chat: "test::c1"})
	if !errors.Is(causeAtStop, errCancelledByUser) || !errors.Is(causeGiven, errCancelledByUser) {
		t.Fatalf("cause when the run was stopped = %v, given to it = %v; want the /cancel's", causeAtStop, causeGiven)
	}
	if h.running() {
		t.Fatal("the stopped run still reads as running")
	}
}

// panickingSTI is a session token issuer whose Issue panics.
type panickingSTI struct{ countingSTI }

func (s *panickingSTI) Issue(_, _, _ string) string { panic("token store unavailable") }

// cancelCLITurn cancels, through the session's /cancel, a turn of Alice on
// model whose run has finished already or is still running, with sti as
// the loop's token issuer. It returns the number of tokens sti had issued
// when the run was stopped and the messages the cancel dropped.
func cancelCLITurn(t *testing.T, sti SessionTokenIssuer, model string, finished bool) (issuedAtStop int, dropped []bus.InboundMessage) {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "Claude CLI", Protocol: "claude-cli"}},
		Models:    []config.ModelConfig{{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true}},
	}
	al := &AgentLoop{cfg: cfg, sessionTokenIssuer: sti}
	alice := &AgentInstance{ID: "alice"}
	al.contextManagers.Store(alice.ID+":s", &cmEntry{sessionKey: "s"})
	_, ht := al.withHostTurn(context.Background(), alice, processOptions{SessionKey: "s"}, &turnModel{model: model}, nil, nil)

	issuedAtStop = -1
	run := &probeRun{done: make(chan struct{})}
	run.onCancel = func(error) {
		if c, ok := sti.(*countingSTI); ok {
			issuedAtStop = c.count()
		}
		if !finished {
			close(run.done)
		}
	}
	if finished {
		close(run.done)
	}
	_, stopTurn := context.WithCancelCause(context.Background())
	ss := &sessionState{
		turnCancel: stopTurn,
		handles:    &turnHandles{h: &runHandle{al: al, agent: alice, run: run, turn: ht}, claimed: true},
		pending:    []bus.InboundMessage{{Content: "queued"}},
	}
	return issuedAtStop, ss.cancel(&userCancelError{chat: "test::c1"})
}

// A /cancel of a running turn on a CLI model gives the session a new MCP
// session token once the run is stopped; a turn that had already finished,
// or one on a model that is not a CLI, keeps its token.
func TestSessionCancel_CLITurnGetsNewToken(t *testing.T) {
	for _, tt := range []struct {
		name     string
		model    string
		finished bool
		want     int
	}{
		{"running on a CLI model", "cc", false, 1},
		{"finished on a CLI model", "cc", true, 0},
		{"running on another model", "alice-api", false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sti := &countingSTI{}
			atStop, _ := cancelCLITurn(t, sti, tt.model, tt.finished)
			if got := sti.count(); got != tt.want {
				t.Errorf("tokens issued = %d, want %d", got, tt.want)
			}
			if atStop != 0 {
				t.Errorf("tokens issued when the run was stopped = %d, want 0: the token is reissued after the stop", atStop)
			}
		})
	}
}

// A panic while reissuing the token is logged and the /cancel completes: it
// still reports the messages it dropped.
func TestSessionCancel_TokenReissuePanicIsContained(t *testing.T) {
	buf := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(buf))
	_, dropped := cancelCLITurn(t, &panickingSTI{}, "cc", false)
	if len(dropped) != 1 || dropped[0].Content != "queued" {
		t.Errorf("dropped = %+v, want the queued message", dropped)
	}
	logs := buf.String()
	if !strings.Contains(logs, "panic while reissuing the session token on cancel") || !strings.Contains(logs, "token store unavailable") {
		t.Errorf("the panic was not logged:\n%s", logs)
	}
}
