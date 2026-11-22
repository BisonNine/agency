// ClawEh
// License: MIT

package providers

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// lockedBuf is a log sink safe for the CLI's goroutines.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCLIProvider_LogsThroughHostLogger: a CLI provider logs to the host's
// logger, and logs the CLI's output only when logging.log_message_content is
// on, as the host's former spawnllm backend allowed.
func TestCLIProvider_LogsThroughHostLogger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock CLI scripts not supported on Windows")
	}
	script := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'ALICE-SECRET-OUTPUT'\necho 'ALICE-SECRET-ERR' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	prev := logger.GetLogMessageContent()
	t.Cleanup(func() { logger.SetLogMessageContent(prev) })

	for _, content := range []bool{false, true} {
		t.Run(map[bool]string{false: "content off", true: "content on"}[content], func(t *testing.T) {
			var out lockedBuf
			restore := logger.RedirectForTest(&out)
			defer restore()
			logger.SetLogMessageContent(content)

			p, model, err := CreateProviderFromConfig(
				&config.ModelConfig{ModelName: "cli", Model: "m", Workspace: t.TempDir()},
				&config.Provider{Name: "cli", Protocol: "claude-cli", Command: script, BypassRestrictions: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Chat(context.Background(), testIdentity, []Message{{Role: "user", Content: "hi"}}, nil, model, nil); err == nil {
				t.Fatal("a CLI that exits 3 succeeded")
			}
			got := out.String()
			if !strings.Contains(got, `"caller":"provider `) {
				t.Fatalf("no provider log line reached the host logger:\n%s", got)
			}
			if has := strings.Contains(got, "ALICE-SECRET"); has != content {
				t.Errorf("CLI output in the log = %v, want %v:\n%s", has, content, got)
			}
		})
	}
}
