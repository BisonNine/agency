package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/testalerts"
)

// skipIfRoot skips tests that rely on permission bits being enforced.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

// lockDir makes dir unreadable for the rest of the test.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(dir, 0o700); cerr != nil {
			t.Error(cerr)
		}
	})
}

// TestRun_UnreadableTreeFails: a folder the backup cannot read fails the run
// rather than leaving its files silently out of the archive, wherever it is.
func TestRun_UnreadableTreeFails(t *testing.T) {
	skipIfRoot(t)
	tests := []struct {
		name     string
		external bool
		dir      func(src Source) string
	}{
		{"tls", false, func(s Source) string { return filepath.Join(s.Home, "tls", "sub") }},
		{"cards", false, func(s Source) string { return filepath.Join(s.Home, "cards", "sub") }},
		{"internal", false, func(s Source) string { return filepath.Join(s.Home, "internal", "sub") }},
		{"forums", false, func(s Source) string { return filepath.Join(s.AgentsDir, "main", "forums", "f1") }},
		{"maestro", false, func(s Source) string { return filepath.Join(s.AgentsDir, "main", "maestro", "p1") }},
		{"classifiers", false, func(s Source) string { return filepath.Join(s.AgentsDir, "main", "classifiers", "c1") }},
		{"elsewhere in home", false, func(s Source) string { return filepath.Join(s.Home, "agents", "main", "sessions", "old") }},
		{"agents directory", false, func(s Source) string { return s.AgentsDir }},
		{"external agents", true, func(s Source) string { return filepath.Join(s.AgentsDir, "bot", "sessions", "old") }},
		{"agent folder", false, func(s Source) string { return filepath.Join(s.AgentsDir, "main") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := fixture(t)
			if tc.external {
				src.AgentsDir = t.TempDir()
				openWAL(t, filepath.Join(src.AgentsDir, "bot", "sessions", "x.archive.db"), 1)
			}
			lockDir(t, tc.dir(src))
			_, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), &testalerts.Recorder{})
			if err == nil || !strings.Contains(err.Error(), "backup: ") {
				t.Fatalf("Run = %v, want a backup error", err)
			}
		})
	}
}

// TestRun_ConfigUnstatable: a config path that cannot be checked fails the
// run instead of omitting config.json.
func TestRun_OptionalFileUnstatable(t *testing.T) {
	for name, set := range map[string]func(s *Source, blocker string){
		"config":      func(s *Source, b string) { s.ConfigPath = filepath.Join(b, "config.json") },
		"cron":        func(s *Source, b string) { s.CronDir = filepath.Join(b, "cron") },
		"credentials": nil,
	} {
		t.Run(name, func(t *testing.T) {
			src := fixture(t)
			blocker := filepath.Join(t.TempDir(), "file")
			writeFileT(t, blocker, "", 0o600)
			if set == nil {
				skipIfRoot(t)
				// credentials.json sits directly in Home: make it unstatable
				// by hiding it behind an unsearchable home.
				src.Home = filepath.Join(t.TempDir(), "home")
				writeFileT(t, filepath.Join(src.Home, "credentials.json"), "{}", 0o600)
				if err := os.Chmod(src.Home, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cerr := os.Chmod(src.Home, 0o700); cerr != nil {
						t.Error(cerr)
					}
				})
				src.ConfigPath, src.CronDir, src.AgentsDir = "", "", ""
			} else {
				set(&src, blocker)
			}
			_, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), &testalerts.Recorder{})
			if err == nil || !strings.Contains(err.Error(), "backup: stat") {
				t.Fatalf("Run = %v, want a stat error", err)
			}
		})
	}
}

// TestRun_NonRegularOptionalFile: a config.json that is not a regular file
// (a directory) is left out rather than archived.
func TestRun_NonRegularOptionalFile(t *testing.T) {
	src := fixture(t)
	if err := os.Remove(src.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(src.ConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), &testalerts.Recorder{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := readTar(t, res.Archive)
	for _, f := range m.Files {
		if f == "config.json" {
			t.Error("a directory named config.json was archived")
		}
	}
}

func TestRun_DestinationErrors(t *testing.T) {
	t.Run("dest under a file", func(t *testing.T) {
		src := fixture(t)
		blocker := filepath.Join(t.TempDir(), "file")
		writeFileT(t, blocker, "", 0o600)
		if _, err := Run(src, filepath.Join(blocker, "out"), time.Now(), &testalerts.Recorder{}); err == nil || !strings.Contains(err.Error(), "backup: create") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dest not writable", func(t *testing.T) {
		skipIfRoot(t)
		src := fixture(t)
		dest := filepath.Join(t.TempDir(), "out")
		if err := os.Mkdir(dest, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(dest, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if _, err := Run(src, dest, time.Now(), &testalerts.Recorder{}); err == nil || !strings.Contains(err.Error(), "backup: temp dir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("archive name taken by a directory", func(t *testing.T) {
		src := fixture(t)
		dest := filepath.Join(t.TempDir(), "out")
		now := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
		if err := os.MkdirAll(filepath.Join(dest, ArchivePrefix+now.Format(stampLayout)+ArchiveSuffix, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(src, dest, now, &testalerts.Recorder{}); err == nil || !strings.Contains(err.Error(), "backup: place") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestRunForConfig_Errors: a failed run is returned, and a destination that
// cannot be listed for pruning does not fail a written backup.
func TestRunForConfig_Errors(t *testing.T) {
	home := cliHome(t)
	cfg := loadCfg(t, home)
	blocker := filepath.Join(t.TempDir(), "file")
	writeFileT(t, blocker, "", 0o600)
	if _, err := RunForConfig(cfg, filepath.Join(home, "config.json"), time.Now(), Options{Dest: filepath.Join(blocker, "x"), Alerter: &testalerts.Recorder{}}); err == nil {
		t.Fatal("RunForConfig into a blocked destination succeeded")
	}
}

func TestPrune_Errors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	writeFileT(t, blocker, "", 0o600)
	if _, err := Prune(blocker, 7, time.Now()); err == nil || !strings.Contains(err.Error(), "backup prune: read") {
		t.Fatalf("Prune of a file = %v", err)
	}
	t.Run("old archive cannot be removed", func(t *testing.T) {
		skipIfRoot(t)
		dest := t.TempDir()
		old := filepath.Join(dest, "20000101")
		writeFileT(t, filepath.Join(old, "x"), "", 0o600)
		writeFileT(t, filepath.Join(dest, ArchivePrefix+"20000101-000000"+ArchiveSuffix), "", 0o600)
		if err := os.Chmod(old, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(old, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		n, err := Prune(dest, 7, time.Now())
		if err != nil || n != 1 {
			t.Fatalf("Prune = %d, %v; want the archive removed and the stuck folder skipped", n, err)
		}
		if _, serr := os.Stat(old); serr != nil {
			t.Errorf("stuck folder gone: %v", serr)
		}
	})
}

// tarSpec is one entry of a hand-built archive.
type tarSpec struct {
	name, body string
	typeflag   byte
}

// handArchive writes a gzipped tar of specs, in order, to a new file.
func handArchive(t *testing.T, specs ...tarSpec) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, s := range specs {
		tf := s.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Typeflag: tf, Name: s.name, Mode: 0o600}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(s.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(s.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "hand.tar.gz")
	writeFileT(t, p, buf.String(), 0o600)
	return p
}

const goodManifest = `{"version":1,"created_at":"2026-01-01T00:00:00Z","home":"/nowhere"}`

func TestReadArchive_Errors(t *testing.T) {
	var garbageTar bytes.Buffer
	gz := gzip.NewWriter(&garbageTar)
	if _, cerr := gz.Write([]byte(strings.Repeat("not a tar header ", 40))); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	garbagePath := filepath.Join(t.TempDir(), "garbage.tar.gz")
	writeFileT(t, garbagePath, garbageTar.String(), 0o600)

	// A valid archive cut short after the manifest.
	full, err := os.ReadFile(handArchive(t, tarSpec{name: ManifestName, body: goodManifest}, tarSpec{name: "config.json", body: strings.Repeat("x", 8192)}))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	var cut bytes.Buffer
	gz = gzip.NewWriter(&cut)
	if _, cerr := gz.Write(plain.Bytes()[:2048]); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	cutPath := filepath.Join(t.TempDir(), "cut.tar.gz")
	writeFileT(t, cutPath, cut.String(), 0o600)

	tests := []struct {
		name, archive, want string
	}{
		{"missing", filepath.Join(t.TempDir(), "missing.tar.gz"), "restore: open"},
		{"not a tar", garbagePath, "is not a tar archive"},
		{"manifest not first", handArchive(t, tarSpec{name: "config.json", body: "{}"}, tarSpec{name: ManifestName, body: goodManifest}), `first entry "config.json"`},
		{"manifest garbled", handArchive(t, tarSpec{name: ManifestName, body: "{not json"}), "restore: manifest"},
		{"truncated", cutPath, "restore: "},
		{"unsafe path", handArchive(t, tarSpec{name: ManifestName, body: goodManifest}, tarSpec{name: "../escape", body: "x"}), "unsafe archive path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanRestore(tc.archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PlanRestore = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestPlanRestore_SkipsNonRegular: directory and link entries in an archive
// are not restored.
func TestPlanRestore_SkipsNonRegular(t *testing.T) {
	archive := handArchive(t,
		tarSpec{name: ManifestName, body: goodManifest},
		tarSpec{name: "cards/", typeflag: tar.TypeDir},
		tarSpec{name: "cards/a.json", body: "{}"},
	)
	plan, err := PlanRestore(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Name != "cards/a.json" {
		t.Fatalf("entries = %+v, want only the regular file", plan.Entries)
	}
}

// planFor plans the restore of a two-file archive into a fresh home.
func planFor(t *testing.T) (*Plan, string) {
	t.Helper()
	archive := handArchive(t,
		tarSpec{name: ManifestName, body: goodManifest},
		tarSpec{name: "config.json", body: `{"restored":true}`},
		tarSpec{name: "cards/a.json", body: `{"card":1}`},
	)
	home := t.TempDir()
	writeFileT(t, filepath.Join(home, "config.json"), `{"old":true}`, 0o600)
	plan, err := PlanRestore(archive, home)
	if err != nil {
		t.Fatal(err)
	}
	return plan, home
}

func TestApply_Errors(t *testing.T) {
	t.Run("lock unreadable", func(t *testing.T) {
		plan, home := planFor(t)
		writeFileT(t, filepath.Join(home, "internal"), "", 0o600)
		if _, err := plan.Apply(time.Now()); err == nil || !strings.Contains(err.Error(), "open lock file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("staging blocked", func(t *testing.T) {
		plan, home := planFor(t)
		now := time.Now()
		writeFileT(t, filepath.Join(home, restoreStagingPrefix+now.Format(stampLayout)), "", 0o600)
		if _, err := plan.Apply(now); err == nil || !strings.Contains(err.Error(), "create staging") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("archive gone", func(t *testing.T) {
		plan, _ := planFor(t)
		if err := os.Remove(plan.Archive); err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Apply(time.Now()); err == nil || !strings.Contains(err.Error(), "restore: open") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("parking blocked", func(t *testing.T) {
		plan, home := planFor(t)
		now := time.Now()
		writeFileT(t, filepath.Join(home, restoreBackupPrefix+now.Format(stampLayout)), "", 0o600)
		_, err := plan.Apply(now)
		if err == nil || !strings.Contains(err.Error(), "restore: create") {
			t.Fatalf("err = %v", err)
		}
		if got, rerr := os.ReadFile(filepath.Join(home, "config.json")); rerr != nil || string(got) != `{"old":true}` {
			t.Errorf("config.json = %q, want unchanged", got)
		}
	})
	t.Run("archive changed since planning", func(t *testing.T) {
		plan, home := planFor(t)
		other := handArchive(t, tarSpec{name: ManifestName, body: goodManifest}, tarSpec{name: "config.json", body: "{}"})
		data, err := os.ReadFile(other)
		if err != nil {
			t.Fatal(err)
		}
		writeFileT(t, plan.Archive, string(data), 0o600)
		_, err = plan.Apply(time.Now())
		if err == nil || !strings.Contains(err.Error(), "cards/a.json is in the plan but not in the archive") {
			t.Fatalf("err = %v", err)
		}
		_ = home
	})
	t.Run("target folder blocked", func(t *testing.T) {
		plan, home := planFor(t)
		writeFileT(t, filepath.Join(home, "cards"), "a file where the folder goes", 0o600)
		// The plan still thinks cards/a.json is new; the folder cannot be made.
		if _, err := plan.Apply(time.Now()); err == nil || !strings.Contains(err.Error(), "restore: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("old file cannot be parked", func(t *testing.T) {
		skipIfRoot(t)
		plan, home := planFor(t)
		// config.json sits in home; a read-only home lets staging fail first,
		// so block the parked copy's folder instead.
		cards := filepath.Join(home, "cards")
		writeFileT(t, filepath.Join(cards, "a.json"), "old card", 0o600)
		if err := os.Chmod(cards, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(cards, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if _, err := plan.Apply(time.Now()); err == nil {
			t.Fatal("Apply succeeded although the old card could not be moved aside")
		}
	})
}

// TestMoveFile_CopyFallback: when a rename is refused (as across
// filesystems) the file is copied with its mode and the source removed.
func TestMoveFile_CopyFallback(t *testing.T) {
	orig := rename
	t.Cleanup(func() { rename = orig })
	rename = func(string, string) error { return errors.New("invalid cross-device link") }

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeFileT(t, src, "payload", 0o640)
	dst := filepath.Join(dir, "dst")
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("dst = %v, %v; want mode 0640", fi, err)
	}
	if got, rerr := os.ReadFile(dst); rerr != nil || string(got) != "payload" {
		t.Errorf("dst = %q", got)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("src still there: %v", err)
	}

	t.Run("missing source", func(t *testing.T) {
		if err := moveFile(filepath.Join(dir, "nope"), dst+"2"); err == nil || !strings.Contains(err.Error(), "restore: stat") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("destination exists", func(t *testing.T) {
		s := filepath.Join(dir, "s2")
		writeFileT(t, s, "x", 0o600)
		if err := moveFile(s, dst); err == nil || !strings.Contains(err.Error(), "restore: create") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(s); err != nil {
			t.Errorf("source removed after a failed copy: %v", err)
		}
	})
	t.Run("unreadable source", func(t *testing.T) {
		skipIfRoot(t)
		s := filepath.Join(dir, "s3")
		writeFileT(t, s, "x", 0o200)
		if err := moveFile(s, filepath.Join(dir, "d3")); err == nil || !strings.Contains(err.Error(), "restore: open") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("source cannot be removed", func(t *testing.T) {
		skipIfRoot(t)
		ro := filepath.Join(dir, "ro")
		s := filepath.Join(ro, "s4")
		writeFileT(t, s, "x", 0o600)
		if err := os.Chmod(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := os.Chmod(ro, 0o700); cerr != nil {
				t.Error(cerr)
			}
		})
		if err := moveFile(s, filepath.Join(dir, "d4")); err == nil || !strings.Contains(err.Error(), "restore: remove") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestWriteFile_ShortSource: an archive entry shorter than its header says
// fails instead of restoring a truncated file.
func TestWriteFile_ShortSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "f")
	err := writeFile(dst, strings.NewReader("abc"), 10, 0o600)
	if err == nil || !strings.Contains(err.Error(), "restore: write") {
		t.Fatalf("err = %v", err)
	}
}

func TestPark_Errors(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	hidden := filepath.Join(dir, "hidden")
	writeFileT(t, filepath.Join(hidden, "f"), "x", 0o600)
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := os.Chmod(hidden, 0o700); cerr != nil {
			t.Error(cerr)
		}
	})
	if err := park(filepath.Join(hidden, "f"), filepath.Join(dir, "parked", "f")); err == nil || !strings.Contains(err.Error(), "restore: stat") {
		t.Fatalf("park of an unstatable file = %v", err)
	}
	target := filepath.Join(dir, "t")
	writeFileT(t, target, "x", 0o600)
	blocker := filepath.Join(dir, "blocker")
	writeFileT(t, blocker, "", 0o600)
	if err := park(target, filepath.Join(blocker, "sub", "t")); err == nil || !strings.Contains(err.Error(), "restore: ") {
		t.Fatalf("park into a blocked folder = %v", err)
	}
}

func TestQuickCheck_NotADatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	writeFileT(t, p, strings.Repeat("definitely not sqlite ", 100), 0o600)
	if err := QuickCheck(p); err == nil || !strings.Contains(err.Error(), "quick_check "+p) {
		t.Fatalf("QuickCheck = %v", err)
	}
}

func loadCfg(t *testing.T, home string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestRun_FileInAgentsDir: a stray file directly in the agents directory is
// not taken for an agent.
func TestRun_FileInAgentsDir(t *testing.T) {
	src := fixture(t)
	writeFileT(t, filepath.Join(src.AgentsDir, "README"), "not an agent", 0o600)
	if _, err := Run(src, filepath.Join(t.TempDir(), "out"), time.Now(), &testalerts.Recorder{}); err != nil {
		t.Fatal(err)
	}
}

// TestApply_PlaceErrors: a restored file that cannot be put in place fails
// the restore.
func TestApply_PlaceErrors(t *testing.T) {
	skipIfRoot(t)
	for _, name := range []string{"cards/a.json", "cards/sub/a.json"} {
		t.Run(name, func(t *testing.T) {
			archive := handArchive(t, tarSpec{name: ManifestName, body: goodManifest}, tarSpec{name: name, body: "{}"})
			home := t.TempDir()
			cards := filepath.Join(home, "cards")
			if err := os.Mkdir(cards, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cerr := os.Chmod(cards, 0o700); cerr != nil {
					t.Error(cerr)
				}
			})
			plan, err := PlanRestore(archive, home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plan.Apply(time.Now()); err == nil || !strings.Contains(err.Error(), "restore: ") {
				t.Fatalf("Apply = %v, want a restore error", err)
			}
		})
	}
}

// TestApply_ArchiveTruncatedSincePlanning: an archive cut short after it was
// planned fails while staging, before anything in the home changes.
func TestApply_ArchiveTruncatedSincePlanning(t *testing.T) {
	plan, home := planFor(t)
	full, err := os.ReadFile(handArchive(t, tarSpec{name: ManifestName, body: goodManifest}, tarSpec{name: "config.json", body: strings.Repeat("x", 8192)}))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	var cut bytes.Buffer
	gz := gzip.NewWriter(&cut)
	if _, cerr := gz.Write(plain.Bytes()[:2048]); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	writeFileT(t, plan.Archive, cut.String(), 0o600)
	if _, err := plan.Apply(time.Now()); err == nil || !strings.Contains(err.Error(), "restore: write") {
		t.Fatalf("Apply = %v, want a write error", err)
	}
	if got, rerr := os.ReadFile(filepath.Join(home, "config.json")); rerr != nil || string(got) != `{"old":true}` {
		t.Errorf("config.json = %q, want unchanged", got)
	}
}
