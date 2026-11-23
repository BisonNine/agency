// ClawEh
// License: MIT

package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
)

// backupCfg loads a config for a temp CLAW_HOME with the backup at 02:30 into
// dest, enabled or not.
func backupCfg(t *testing.T, enabled bool, dest string) (*config.Config, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	path := filepath.Join(home, "config.json")
	cfg := config.DefaultConfig()
	cfg.Backup.Enabled = &enabled
	cfg.Backup.At = "02:30"
	cfg.Backup.Dest = dest
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, path
}

func archives(t *testing.T, dest string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dest, "claw-backup-*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// TestNightlyBackup_Tick: the backup runs at the configured minute, once a
// day, only while enabled.
func TestNightlyBackup_Tick(t *testing.T) {
	dest := t.TempDir()
	cfg, path := backupCfg(t, true, dest)
	rec := &alertRecorder{}
	nb := &nightlyBackup{getConfig: func() *config.Config { return cfg }, configPath: path, alerter: rec}
	at := time.Date(2026, 3, 4, 2, 30, 0, 0, time.UTC)

	nb.tick(at.Add(-time.Minute))
	if archives(t, dest) != 0 {
		t.Fatal("backup ran before its minute")
	}
	nb.tick(at)
	if archives(t, dest) != 1 {
		t.Fatal("backup did not run at its minute")
	}
	nb.tick(at.Add(20 * time.Second))
	if archives(t, dest) != 1 {
		t.Fatal("backup ran twice on one day")
	}
	nb.tick(at.AddDate(0, 0, 1))
	if archives(t, dest) != 2 {
		t.Fatal("backup did not run the next day")
	}
	if len(rec.titles()) != 0 {
		t.Errorf("alerts = %v", rec.titles())
	}

	disabled, _ := backupCfg(t, false, dest)
	nb.getConfig = func() *config.Config { return disabled }
	nb.tick(at.AddDate(0, 0, 2))
	nb.getConfig = func() *config.Config { return nil }
	nb.tick(at.AddDate(0, 0, 3))
	if archives(t, dest) != 2 {
		t.Fatal("backup ran while disabled or without a config")
	}
}

// TestNightlyBackup_FailureAlerts: a backup that cannot be written alerts.
func TestNightlyBackup_FailureAlerts(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, path := backupCfg(t, true, filepath.Join(blocker, "dest"))
	rec := &alertRecorder{}
	nb := &nightlyBackup{getConfig: func() *config.Config { return cfg }, configPath: path, alerter: rec}
	nb.tick(time.Date(2026, 3, 4, 2, 30, 0, 0, time.UTC))
	if !rec.has("Nightly backup failed") {
		t.Fatalf("alerts = %v", rec.titles())
	}
}

// TestStartBackupScheduler_Stops: the scheduler's stop function ends it.
func TestStartBackupScheduler_Stops(t *testing.T) {
	stop := startBackupScheduler(func() *config.Config { return nil }, "", &alertRecorder{})
	stop()
}
