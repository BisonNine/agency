package gateway

import (
	"context"
	"time"

	"github.com/tenebris-tech/alerter"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/backup"
	"go.bison9.com/agency/logger"
)

// startBackupScheduler runs the optional nightly backup. It ticks once a minute
// and, when backup is enabled and the local clock reaches the configured HH:MM,
// writes the backup archive (once per day). getConfig is read
// live each tick so toggling the feature, the time, or retention takes effect
// without a restart. Returns a stop function.
func startBackupScheduler(getConfig func() *config.Config, configPath string, a alerter.Alerter) func() {
	ctx, cancel := context.WithCancel(context.Background())
	nb := &nightlyBackup{getConfig: getConfig, configPath: configPath, alerter: a}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				nb.tick(now)
			}
		}
	}()
	return cancel
}

// nightlyBackup is the backup scheduler's state between ticks.
type nightlyBackup struct {
	getConfig  func() *config.Config
	configPath string
	alerter    alerter.Alerter
	lastRunDay string // YYYYMMDD of the last run; guards once-per-day
}

// tick runs the backup when it is enabled and now is the configured HH:MM,
// once per day.
func (n *nightlyBackup) tick(now time.Time) {
	cfg := n.getConfig()
	if cfg == nil || !cfg.Backup.IsEnabled() {
		return
	}
	hour, minute := cfg.Backup.BackupAt()
	if now.Hour() != hour || now.Minute() != minute {
		return
	}
	day := now.Format("20060102")
	if day == n.lastRunDay {
		return
	}
	n.lastRunDay = day
	res, err := backup.RunForConfig(cfg, n.configPath, now, backup.Options{Alerter: n.alerter}) //nolint:contextcheck,nolintlint // same code path as `claw backup`, which has no context; the quick_check inside runs on its own busy_timeout-bounded connection
	if err != nil {
		logger.ErrorCF("backup", "nightly backup failed", map[string]any{"error": err.Error()})
		n.alerter.Send(alerter.Alert{
			Title:       "Nightly backup failed",
			Description: "the backup archive was not written; nothing retries before tomorrow",
			Details:     err.Error(),
			EventID:     "backup",
		})
		return
	}
	logger.InfoCF("backup", "nightly backup complete", map[string]any{
		"archive": res.Archive, "bytes": res.Bytes, "files": res.Files, "skipped": len(res.Skipped),
	})
}
