package backup

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/config"
)

// The shell tools' job logs and records are left out of the backup.
func TestRunLeavesOutShellJobs(t *testing.T) {
	src := fixture(t)
	jobs := filepath.Join(src.Home, "agents", "main", filepath.FromSlash(config.ShellJobsDir))
	writeFileT(t, filepath.Join(jobs, "j0123abcd.log"), "build output", 0o600)
	writeFileT(t, filepath.Join(jobs, "j0123abcd.json"), `{"id":"j0123abcd","state":"exited"}`, 0o600)

	res, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, entries := readTar(t, res.Archive)
	for name := range entries {
		if strings.Contains(name, "/"+config.ShellJobsDir+"/") {
			t.Errorf("%s must not be archived", name)
		}
	}
}
