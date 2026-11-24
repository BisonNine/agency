package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// Backups are agency-backup-<timestamp>.tar.gz and the manifest names the
// data directory agency_home.
func TestBackupNames(t *testing.T) {
	if ArchivePrefix != "agency-backup-" {
		t.Errorf("ArchivePrefix = %q, want agency-backup-", ArchivePrefix)
	}
	b, err := json.Marshal(Manifest{Home: "/srv/alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"agency_home":"/srv/alice"`) {
		t.Errorf("manifest = %s, want the agency_home key", b)
	}
}
