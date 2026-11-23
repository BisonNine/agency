// ClawEh
// License: MIT

package gateway

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/clock"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/testalerts"
	"go.bison9.com/agency/logger"
)

// logLines captures what fn logs.
func logLines(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	restore := logger.RedirectForTest(&buf)
	defer restore()
	fn()
	return buf.String()
}

// TestWarnOtherAccess: each configured mode that opens a file or folder to
// other users is logged by its key.
func TestWarnOtherAccess(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Security.Permissions.Logs = config.AreaPermissions{DirMode: "0755", FileMode: "0644"}
	out := logLines(t, func() { warnOtherAccess(cfg) })
	for _, want := range []string{"security.permissions.logs.dir_mode is 0755", "security.permissions.logs.file_mode is 0644"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// TestOpenAuditLog_Unavailable: a data directory where the audit database
// cannot be opened leaves the gateway running without one, with a warning.
func TestOpenAuditLog_Unavailable(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "internal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := logLines(t, func() { openAuditLog(home) }); !strings.Contains(out, "audit log disabled") {
		t.Errorf("log = %s", out)
	}
}

// TestSavePermissionDefaults_WriteFails: a config file that cannot be
// rewritten is logged and the gateway carries on.
func TestSavePermissionDefaults_WriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(dir, 0o700); cerr != nil {
			t.Error(cerr)
		}
	})
	if out := logLines(t, func() { savePermissionDefaults(store) }); !strings.Contains(out, "could not write the security.permissions defaults") {
		t.Errorf("log = %s", out)
	}
}

// TestConfigWatcher_UnparseableFileAlerts: a config file that no longer
// parses keeps the running config and alerts.
func TestConfigWatcher_UnparseableFileAlerts(t *testing.T) {
	rec := testalerts.Install(t)
	store, path := seedStore(t)
	h := startWatcher(t, store, rec)
	if err := os.WriteFile(path, []byte(`{"models": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ticks(t, 2*debounceTicks)
	h.noReload(t, "an unparseable config was applied")
	got := rec.Alerts()
	if len(got) != 1 || got[0].Title != "Config file invalid" {
		t.Fatalf("alerts = %+v", got)
	}
}

// TestConfigWatcher_EditRevertedIsNoReload: a file edited and put back as it
// was before the change settled is not reloaded (here with debug logging on).
func TestConfigWatcher_EditRevertedIsNoReload(t *testing.T) {
	store, path := seedStore(t)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &watcherHarness{fc: clock.NewFake(time.Now()), polled: make(chan struct{})}
	h.w = newConfigWatcher(store, testPollInterval, testDebounce, true, alerter.Nop{}, &modelRefAlerts{})
	h.w.clock = h.fc
	h.w.polled = h.polled
	h.ch = h.w.out
	h.w.start()
	t.Cleanup(h.w.close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.fc.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, path, "an edit that is undone")
	h.tick(t)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	h.ticks(t, 2*debounceTicks)
	h.noReload(t, "a reverted edit was reloaded")
}
