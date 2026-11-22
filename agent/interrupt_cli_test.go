// ClawEh
// License: MIT

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
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
