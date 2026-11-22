// ClawEh
// License: MIT

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.bison9.com/agent/hook"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/mcpserver"
)

// countingSTI issues numbered tokens and counts them.
type countingSTI struct {
	recordingSTI
	mu     sync.Mutex
	issued int
}

func (s *countingSTI) Issue(_, _, _ string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued++
	return "SST-" + strconv.Itoa(s.issued)
}

func (s *countingSTI) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

// hangingCLI writes a stand-in for the claude binary that records its pid
// and that of a child it starts, then waits forever.
func hangingCLI(t *testing.T, logDir string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\n" +
		"echo $$ > '" + logDir + "/pid.tmp' && mv '" + logDir + "/pid.tmp' '" + logDir + "/pid'\n" +
		"sleep 300 &\n" +
		"echo $! > '" + logDir + "/child.tmp' && mv '" + logDir + "/child.tmp' '" + logDir + "/child'\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// readPID waits for the pid file name under dir.
func readPID(t *testing.T, dir, name string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the CLI wrote no %s", name)
	return 0
}

// processGone waits for pid to be gone (or a zombie nobody waits for).
func processGone(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if b, rerr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); rerr == nil && strings.Contains(string(b), ") Z ") {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Rule 1 on a CLI model: /cancel kills the CLI's process group and replaces
// the turn's MCP session token.
func TestInterrupt_CLIModelKilledAndTokenReplaced(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	logDir := t.TempDir()
	cfg := ownerTestConfig(t)
	cfg.Providers = []config.Provider{{Name: "Claude CLI", Protocol: "claude-cli", Command: hangingCLI(t, logDir)}}
	cfg.Models = []config.ModelConfig{{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true}}
	cfg.Agents.Defaults.Models = []string{"cc"}
	if err := os.MkdirAll(cfg.CLIPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	msgBus := bus.NewMessageBus()
	al := mustNewAgentLoop(t, cfg, msgBus, &recordingProvider{}, realClients{}, OwnsDataDir())
	sti := &countingSTI{}
	al.SetSessionTokenIssuer(sti)

	go dispatch(al, inbound("c1", "id1", "hello"))
	pid := readPID(t, logDir, "pid")
	child := readPID(t, logDir, "child")
	issuedBefore := sti.count()

	dispatch(al, inbound("c1", "id2", "/cancel"))
	nextOutbound(t, msgBus)
	al.activeRequests.Wait()

	if !processGone(pid) || !processGone(child) {
		t.Fatalf("the CLI's processes are still running (%d, %d)", pid, child)
	}
	if sti.count() != issuedBefore+1 {
		t.Fatalf("tokens issued = %d, want one more than %d: the interrupted turn's token was not replaced", sti.count(), issuedBefore)
	}
}

// argsCLI is hangingCLI that also records its arguments and standard input.
func argsCLI(t *testing.T, logDir string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + logDir + "/args'\n" +
		"cat > '" + logDir + "/stdin'\n" +
		"echo $$ > '" + logDir + "/pid.tmp' && mv '" + logDir + "/pid.tmp' '" + logDir + "/pid'\n" +
		"sleep 300 &\n" +
		"echo $! > '" + logDir + "/child.tmp' && mv '" + logDir + "/child.tmp' '" + logDir + "/child'\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// Rules 5.2 items 1-2 for a cancelled task (agent_cancel, /cancel all): a
// sub-agent on a CLI model is stopped with its process group, and its MCP
// session token is refused from then on.
func TestCancelTask_CLIWorkerKilledAndTokenRevoked(t *testing.T) {
	restore := logger.RedirectForTest(&safeBufLoop{})
	defer restore()

	logDir := t.TempDir()
	cfg := ownerTestConfig(t)
	cfg.Agents.List[0].Tools = []string{"agent_spawn"}
	cfg.Providers = []config.Provider{{Name: "Claude CLI", Protocol: "claude-cli", Command: argsCLI(t, logDir)}}
	cfg.Models = []config.ModelConfig{{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Enabled: true}}
	cfg.Agents.Defaults.Models = []string{"cc"}
	if err := os.MkdirAll(cfg.CLIPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	al := mustNewAgentLoop(t, cfg, bus.NewMessageBus(), &recordingProvider{}, realClients{}, OwnsDataDir())
	store := mcpserver.NewSessionTokenStore()
	al.SetSessionTokenIssuer(store)
	main, ok := al.GetRegistry().Get("main")
	if !ok || main.spawnMgr == nil {
		t.Fatal("main has no spawn manager")
	}

	cancelled := make(chan struct{}, 4)
	al.tasks.Observe(func(info hook.TaskInfo) {
		if info.State == hook.TaskCancelled {
			cancelled <- struct{}{}
		}
	})
	id, err := main.spawnMgr.SpawnCallback("do it", "job", "", "cli", "direct", "", "", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pid := readPID(t, logDir, "pid")
	child := readPID(t, logDir, "child")
	var token string
	for _, f := range []string{"args", "stdin"} {
		b, err := os.ReadFile(filepath.Join(logDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if m := regexp.MustCompile(`SST[0-9a-f]{64}`).Find(b); m != nil {
			token = string(m)
		}
	}
	if _, ok := store.Resolve(token); token == "" || !ok {
		t.Fatalf("the worker's token %q does not resolve before the cancel", token)
	}

	if res := main.spawnMgr.CancelTask(id); res.Cancelled != 1 {
		t.Fatalf("cancel = %+v", res)
	}
	if !processGone(pid) || !processGone(child) {
		t.Fatalf("the worker's CLI is still running (%d, %d)", pid, child)
	}
	recv(t, cancelled, "the cancel")
	recv(t, cancelled, "the worker to unwind")
	if _, ok := store.Resolve(token); ok {
		t.Fatal("the cancelled worker's token is still accepted")
	}
}
