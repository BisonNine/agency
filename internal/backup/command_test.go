package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/testalerts"
)

// cliHome is a AGENCY_HOME for the commands: a config whose agents live under
// the home, one live database and one plain file.
func cliHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	writeFileT(t, filepath.Join(home, "config.json"),
		`{"agents":{"base_dir":"`+filepath.Join(home, "agents")+`"},"backup":{"retain_days":7}}`, 0o600)
	writeFileT(t, filepath.Join(home, "credentials.json"), `{"k":"v"}`, 0o600)
	openWAL(t, filepath.Join(home, "agents", "main", "sessions", "s1.archive.db"), 3)
	return home
}

// errWriter fails every write.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func runCmd(t *testing.T, args []string, stdin string) (string, string, error) {
	t.Helper()
	cmd := NewBackupCommand()
	if len(args) > 0 && args[0] == "restore" {
		cmd = NewRestoreCommand()
		args = args[1:]
	}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

var wroteRE = regexp.MustCompile(`^Wrote (\S+agency-backup-\d{8}-\d{6}\.tar\.gz) \(\d+(\.\d)? (B|KiB|MiB)\, (\d+) files\)\n$`)

// TestBackupCommand: `agency backup --dest` writes the archive there and says
// where, how big and how many files.
func TestBackupCommand(t *testing.T) {
	cliHome(t)
	dest := filepath.Join(t.TempDir(), "out")
	// An archive past retention is pruned by the same run.
	writeFileT(t, filepath.Join(dest, "agency-backup-20000101-000000.tar.gz"), "old", 0o600)
	out, errOut, err := runCmd(t, []string{"--dest", dest}, "")
	if err != nil {
		t.Fatalf("backup: %v (%s)", err, errOut)
	}
	m := wroteRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output = %q", out)
	}
	if filepath.Dir(m[1]) != dest {
		t.Errorf("archive %s not in %s", m[1], dest)
	}
	manifest, entries := readTar(t, m[1])
	if _, ok := entries["agents/main/sessions/s1.archive.db"]; !ok {
		t.Errorf("database missing from the archive: %v", manifest.Files)
	}
	if _, serr := os.Stat(filepath.Join(dest, "agency-backup-20000101-000000.tar.gz")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("archive past retention not pruned: %v", serr)
	}
}

// TestBackupCommand_DefaultDest: without --dest the archive goes to
// <AGENCY_HOME>/backup.
func TestBackupCommand_DefaultDest(t *testing.T) {
	home := cliHome(t)
	out, _, err := runCmd(t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, filepath.Join(home, "backup", "agency-backup-")) {
		t.Errorf("output = %q, want an archive under the home's backup folder", out)
	}
}

// TestBackupCommand_SkippedDatabase: a database failing quick_check is named
// on stderr and the command fails, though the archive is written.
func TestBackupCommand_SkippedDatabase(t *testing.T) {
	rec := testalerts.Install(t)
	home := cliHome(t)
	bad := filepath.Join(home, "internal", "broken.db")
	writeFileT(t, bad, "garbage garbage garbage garbage garbage", 0o600)
	out, errOut, err := runCmd(t, []string{"--dest", t.TempDir()}, "")
	if err == nil || err.Error() != "1 database(s) failed quick_check and were left out of the archive" {
		t.Fatalf("err = %v", err)
	}
	if !wroteRE.MatchString(out) {
		t.Errorf("archive not reported: %q", out)
	}
	if !strings.HasPrefix(errOut, "SKIPPED "+bad+": ") {
		t.Errorf("stderr = %q", errOut)
	}
	if len(rec.Alerts()) != 1 {
		t.Errorf("alerts = %+v, want 1", rec.Alerts())
	}
}

func TestBackupCommand_Errors(t *testing.T) {
	t.Run("config unreadable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		writeFileT(t, filepath.Join(home, "config.json"), "{not json", 0o600)
		if _, _, err := runCmd(t, nil, ""); err == nil || !strings.Contains(err.Error(), "load config") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("destination blocked", func(t *testing.T) {
		cliHome(t)
		blocker := filepath.Join(t.TempDir(), "file")
		writeFileT(t, blocker, "", 0o600)
		if _, _, err := runCmd(t, []string{"--dest", filepath.Join(blocker, "out")}, ""); err == nil || !strings.Contains(err.Error(), "backup: create") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("output fails", func(t *testing.T) {
		cliHome(t)
		cmd := NewBackupCommand()
		cmd.SetOut(errWriter{})
		cmd.SetArgs([]string{"--dest", t.TempDir()})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || err.Error() != "write refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stderr fails", func(t *testing.T) {
		testalerts.Install(t)
		home := cliHome(t)
		writeFileT(t, filepath.Join(home, "internal", "broken.db"), "garbage garbage garbage garbage garbage", 0o600)
		cmd := NewBackupCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(errWriter{})
		cmd.SetArgs([]string{"--dest", t.TempDir()})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || err.Error() != "write refused" {
			t.Fatalf("err = %v", err)
		}
	})
}

// backupOf writes an archive of a fresh home (with an external agents
// directory when external) and returns it.
func backupOf(t *testing.T, external bool) string {
	t.Helper()
	src := fixture(t)
	if external {
		ext := t.TempDir()
		openWAL(t, filepath.Join(ext, "bot", "sessions", "x.archive.db"), 2)
		src.AgentsDir = ext
	}
	res, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), &testalerts.Recorder{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Archive
}

// TestRestoreCommand: the plan is shown with new and replaced files, "yes"
// applies it, and the replaced files are parked.
func TestRestoreCommand(t *testing.T) {
	archive := backupOf(t, false)
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	writeFileT(t, filepath.Join(home, "config.json"), `{"old":true}`, 0o600)

	out, _, err := runCmd(t, []string{"restore", archive}, "yes\n")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Archive:   " + archive,
		"Restoring into " + home,
		"  REPLACE  " + filepath.Join(home, "config.json"),
		"  new      " + filepath.Join(home, "credentials.json"),
		" replaced; replaced files are kept under " + filepath.Join(home, restoreBackupPrefix+"<timestamp>"),
		"Type 'yes' to continue: ",
		"Restored ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Agent workspaces into") {
		t.Errorf("agents line printed without an external agents directory:\n%s", out)
	}
	if got, rerr := os.ReadFile(filepath.Join(home, "config.json")); rerr != nil || string(got) != `{"agents":{}}` {
		t.Errorf("config.json = %q (%v), want the archived one", got, rerr)
	}
	parked, rerr := filepath.Glob(filepath.Join(home, restoreBackupPrefix+"*", "config.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(parked) != 1 {
		t.Fatalf("parked config = %v", parked)
	}
	if got, rerr := os.ReadFile(parked[0]); rerr != nil || string(got) != `{"old":true}` {
		t.Errorf("parked config = %q", got)
	}
}

// TestRestoreCommand_ExternalAgents: an archive with an external agents
// directory names it, and --yes skips the question.
func TestRestoreCommand_ExternalAgents(t *testing.T) {
	archive := backupOf(t, true)
	plan, err := PlanRestore(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Restore over the original agents directory's place, which is empty now.
	if ierr := os.RemoveAll(plan.Manifest.AgentsDir); ierr != nil {
		t.Fatal(ierr)
	}
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	out, _, err := runCmd(t, []string{"restore", "--yes", archive}, "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(out, "Agent workspaces into "+plan.Manifest.AgentsDir) || strings.Contains(out, "Type 'yes'") {
		t.Errorf("output:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(plan.Manifest.AgentsDir, "bot", "sessions", "x.archive.db")); serr != nil {
		t.Errorf("external agent database not restored: %v", serr)
	}
}

func TestRestoreCommand_NotConfirmed(t *testing.T) {
	archive := backupOf(t, false)
	for _, tc := range []struct{ stdin, want string }{
		{"no\n", "restore cancelled"},
		{"y", "restore cancelled"},
		{"", "no confirmation read; re-run with --yes"},
	} {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		_, _, err := runCmd(t, []string{"restore", archive}, tc.stdin)
		if err == nil || err.Error() != tc.want {
			t.Errorf("stdin %q: err = %v, want %q", tc.stdin, err, tc.want)
		}
		if entries, rerr := os.ReadDir(home); rerr != nil || len(entries) != 0 {
			t.Errorf("stdin %q: home changed without confirmation: %v", tc.stdin, entries)
		}
	}
}

func TestRestoreCommand_Errors(t *testing.T) {
	archive := backupOf(t, false)
	t.Run("gateway running", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		holdLock(t, home)
		if _, _, err := runCmd(t, []string{"restore", "--yes", archive}, ""); !errors.Is(err, ErrGatewayRunning) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("lock unreadable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		writeFileT(t, filepath.Join(home, "internal"), "", 0o600) // a file where the directory belongs
		if _, _, err := runCmd(t, []string{"restore", "--yes", archive}, ""); err == nil || !strings.Contains(err.Error(), "open lock file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not an archive", func(t *testing.T) {
		t.Setenv(global.EnvVarHome, t.TempDir())
		if _, _, err := runCmd(t, []string{"restore", "--yes", filepath.Join(t.TempDir(), "missing.tar.gz")}, ""); err == nil {
			t.Fatal("restore of a missing archive succeeded")
		}
	})
	t.Run("output fails", func(t *testing.T) {
		t.Setenv(global.EnvVarHome, t.TempDir())
		cmd := NewRestoreCommand()
		cmd.SetOut(errWriter{})
		cmd.SetArgs([]string{"--yes", archive})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || err.Error() != "write refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("prompt fails", func(t *testing.T) {
		t.Setenv(global.EnvVarHome, t.TempDir())
		cmd := NewRestoreCommand()
		w := &limitWriter{ok: 1}
		cmd.SetOut(w)
		cmd.SetArgs([]string{archive})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || err.Error() != "write refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("apply fails", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		// A directory where config.json goes cannot be replaced by a file.
		if err := os.MkdirAll(filepath.Join(home, "config.json", "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		if err := os.Chmod(home, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(home, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if _, _, err := runCmd(t, []string{"restore", "--yes", archive}, ""); err == nil {
			t.Fatal("restore into a read-only home succeeded")
		}
	})
}

// limitWriter accepts ok writes, then fails.
type limitWriter struct{ ok int }

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.ok > 0 {
		w.ok--
		return len(p), nil
	}
	return 0, errors.New("write refused")
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		5 << 20:    "5.0 MiB",
		3 << 30:    "3.0 GiB",
		2048 << 30: "2048.0 GiB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
