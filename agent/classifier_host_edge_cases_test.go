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

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/classifier"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/audit"
	"go.bison9.com/agency/logger"
)

// nilResultTool returns no result at all.
type nilResultTool struct{ classifierTool }

func (nilResultTool) Execute(context.Context, map[string]any) *toolkit.Result { return nil }

// A host bound to a loop with no configuration, or none with no registry,
// refuses as not ready and lists no scopes.
func TestClassifierHost_LoopWithoutState(t *testing.T) {
	ctx := context.Background()
	h := NewClassifierHost()
	h.Bind(&AgentLoop{})
	if _, _, err := h.Complete(ctx, "alice", "s", "u"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("Complete with no configuration = %v", err)
	}
	if err := h.RunFailed(ctx, "alice", "x"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("RunFailed with no configuration = %v", err)
	}

	h.Bind(&AgentLoop{cfg: &config.Config{}})
	if got := h.Scopes(); got != nil {
		t.Errorf("Scopes with no registry = %v", got)
	}
	if err := h.Has("alice", "inbox_list"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("Has with no registry = %v", err)
	}
	al := h.loop.Load()
	if _, _, err := h.isolatedFolder(ctx, al, al.cfg, "alice"); !errors.Is(err, errLoopNotReady) {
		t.Errorf("isolatedFolder with no registry = %v", err)
	}
}

// Schema refuses what Has refuses; a tool that returns no result is reported
// without the tool's name (the classifier names it).
func TestClassifierHost_SchemaAndNoResult(t *testing.T) {
	al, host, _ := newClassifierHostLoop(t, classifierHostConfig(t, "http://127.0.0.1:0"))
	if _, err := host.Schema("alice", "nosuch"); err == nil || err.Error() != "tool nosuch is not available to agent alice" {
		t.Errorf("Schema(nosuch) = %v", err)
	}
	registerOn(t, al, "alice", &nilResultTool{classifierTool{name: "inbox_void"}})
	_, err := host.Call(context.Background(), "alice", "inbox_void", map[string]any{})
	if err == nil || strings.Contains(err.Error(), "inbox_void") || errors.Is(err, classifier.ErrToolUnavailable) {
		t.Errorf("Call(inbox_void) = %v, want the reason without the tool's name", err)
	}
}

// A CLI classifier model whose folder can't be made fails the call with the
// folder's error and no model call.
func TestClassifierHost_CLIFolderError(t *testing.T) {
	logDir := t.TempDir()
	cfg := classifierHostConfig(t, "http://127.0.0.1:0")
	cfg.Providers = append(cfg.Providers, modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", Command: fakeClaudeCLI(t, logDir)})
	cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true})
	cfg.Agents.List[0].Models = []string{"cc"}
	cfg.Agents.List[0].Classifiers.Model = "cc"
	al, host, _ := newClassifierHostLoop(t, cfg)
	root := al.GetRegistry().TempRoot()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.Complete(context.Background(), "alice", "s", "u"); err == nil || !strings.HasPrefix(err.Error(), "folder for an isolated CLI call: ") {
		t.Errorf("Complete with the temporary root a file = %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "args")); !os.IsNotExist(err) {
		t.Errorf("the CLI ran: %v", err)
	}
}

// A loop that has not made its temporary root has no folder for a CLI call.
func TestClassifierHost_IsolatedFolderNoRoot(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&safeBufLoop{}))
	al := mustNewAgentLoop(t, newTestConfig(t), bus.NewMessageBus(), &mockProvider{}, realClients{}) // not the data directory's owner
	if root := al.GetRegistry().TempRoot(); root != "" {
		t.Fatalf("a non-owner loop has the temporary root %s before any temporary agent", root)
	}
	h := NewClassifierHost()
	if _, _, err := h.isolatedFolder(context.Background(), al, al.GetConfig(), "alice"); err == nil || err.Error() != "no folder for an isolated CLI call in this process" {
		t.Errorf("isolatedFolder with no root = %v", err)
	}
}

// A failed model call that reports a cost still counts toward the daily
// spend.
func TestClassifierHost_FailedCallCost(t *testing.T) {
	script := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\ncat > /dev/null\n" +
		"printf '%s' '{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"boom\",\"total_cost_usd\":1.5}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := classifierHostConfig(t, "http://127.0.0.1:0")
	cfg.Providers = append(cfg.Providers, modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", Command: script})
	cfg.Models = append(cfg.Models, modelspec.Model{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true})
	cfg.Agents.List[0].Models = []string{"cc"}
	cfg.Agents.List[0].Classifiers.Model = "cc"
	cfg.Agents.Defaults.DailySpendAlertUSD = 1.0
	al, host, _ := newClassifierHostLoop(t, cfg)
	rec := &alertRecorder{}
	al.SetAlerter(rec)
	if _, _, err := host.Complete(context.Background(), "alice", "s", "u"); err == nil {
		t.Fatal("Complete on a failing model succeeded")
	}
	rec.mu.Lock()
	n := len(rec.alerts)
	rec.mu.Unlock()
	if n != 1 {
		t.Errorf("alerts after a failed $1.50 call = %d, want 1", n)
	}
}

// A summary the chat could not take is logged naming the classifier's agent.
func TestClassifierHost_SummaryNotDelivered(t *testing.T) {
	cfg := classifierHostConfig(t, "http://127.0.0.1:0")
	cfg.Bindings = []config.AgentBinding{{
		AgentID: "alice", Default: true,
		Match: config.BindingMatch{Channel: "telegram", Peer: &config.PeerMatch{Kind: "direct", ID: "u1"}},
	}}
	_, host, msgBus := newClassifierHostLoop(t, cfg)
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	if err := host.Summary(context.Background(), "alice", "done"); err != nil {
		t.Fatal(err)
	}
	out := nextOutbound(t, msgBus)
	out.OnDelivery(nil)
	if strings.Contains(logs.String(), "Classifier summary not delivered") {
		t.Error("a delivered summary was logged as not delivered")
	}
	out.OnDelivery(errors.New("chat not found"))
	if got := logs.String(); !strings.Contains(got, "Classifier summary not delivered") || !strings.Contains(got, "chat not found") {
		t.Errorf("undelivered summary log = %q", got)
	}
}

// A notice the bus can't take is reported naming the agent.
func TestClassifierHost_RunFailedBusClosed(t *testing.T) {
	_, host, msgBus := newClassifierHostLoop(t, classifierHostConfig(t, "http://127.0.0.1:0"))
	msgBus.Close()
	if err := host.RunFailed(context.Background(), "alice", "the run failed"); err == nil || !strings.HasPrefix(err.Error(), "queue the notice for agent alice: ") {
		t.Errorf("RunFailed on a closed bus = %v", err)
	}
}

// With no audit store Record does nothing; a definition write that did not
// succeed is an error row.
func TestClassifierHost_RecordOutcomes(t *testing.T) {
	host := NewClassifierHost()
	host.Record(classifier.AuditEvent{Kind: classifier.AuditWrite, Agent: "alice", Classifier: "x"}) // no store: nothing, no panic

	initAudit(t)
	host.Record(classifier.AuditEvent{Kind: classifier.AuditWrite, Agent: "alice", Classifier: "inbox-sort", Outcome: "refused", Clone: "1a2b3c4d"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := audit.Default().Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := audit.Default().Query(context.Background(), audit.Filter{Kind: audit.KindClassifierWrite, Agent: "alice"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].Outcome != audit.OutcomeError || !strings.Contains(rows[0].Details, `"clone":"1a2b3c4d"`) {
		t.Errorf("row = %+v", rows[0])
	}
}
