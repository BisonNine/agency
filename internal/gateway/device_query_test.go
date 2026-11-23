package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
)

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// openFDs counts this process's open descriptors where /proc exposes them;
// -1 elsewhere.
func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// History for a session key whose agent is not registered reads nothing: no
// session database is created and no descriptor is left open.
func TestDeviceQuerierHistoryUnknownAgent(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			BaseDir: base,
			Defaults: config.AgentDefaults{
				Models:            []string{"test-model"},
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
			List: []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}},
		},
	}
	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	q := deviceAgentQuerier{al: al}

	filesBefore, fdsBefore := countFiles(t, base), openFDs()
	for range 50 {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		if got := q.History("agent:" + hex.EncodeToString(b) + ":main"); got != nil {
			t.Fatalf("history for an unknown agent: %+v", got)
		}
	}
	if after := countFiles(t, base); after != filesBefore {
		t.Fatalf("files under the agents dir: %d before, %d after", filesBefore, after)
	}
	if fdsBefore >= 0 {
		if after := openFDs(); after > fdsBefore {
			t.Fatalf("open descriptors: %d before, %d after", fdsBefore, after)
		}
	}
}

// TestDeviceQuerierHistoryFilters: only user and assistant turns with text
// are returned; the default agent is the registry's.
func TestDeviceQuerierHistoryFilters(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			BaseDir:  t.TempDir(),
			Defaults: config.AgentDefaults{Models: []string{"test-model"}, MaxTokens: 4096, MaxToolIterations: 10},
			List:     []config.AgentConfig{{ID: "alice", Name: "Alice", Default: true}},
		},
	}
	al, err := agent.NewAgentLoop(t.Context(), cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	t.Cleanup(func() { al.Close(t.Context()) })
	q := deviceAgentQuerier{al: al}
	if q.DefaultAgentID() != "alice" {
		t.Errorf("DefaultAgentID = %q", q.DefaultAgentID())
	}
	inst, ok := al.GetRegistry().GetConfigured("alice")
	if !ok {
		t.Fatal("alice not registered")
	}
	const key = "agent:alice:main"
	for _, m := range [][2]string{{"user", "hello"}, {"system", "rules"}, {"assistant", "  "}, {"assistant", "hi there"}} {
		if err := inst.Sessions.AddMessage(key, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	got := q.History(key)
	if len(got) != 2 || got[0].Content != "hello" || got[1].Content != "hi there" {
		t.Fatalf("history = %+v", got)
	}
	if q.History("not-an-agent-key") != nil {
		t.Error("history for a key that names no agent")
	}
}

// TestDeviceInjectionsWithoutSupport: a channel without the hooks, no
// certificate and an unusable credentials file are all tolerated.
func TestDeviceInjectionsWithoutSupport(t *testing.T) {
	injectDeviceTLS(nil, nil)
	if got := adminUsername(filepath.Join(t.TempDir(), "missing.json"), config.DefaultFileMode); got != "" {
		t.Errorf("adminUsername without a file = %q", got)
	}
}
