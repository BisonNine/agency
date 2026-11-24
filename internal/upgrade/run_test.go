package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/install"
)

// archiveName is the release archive for the platform the tests run on.
var archiveName = fmt.Sprintf("%s-%s-%s.tar.gz", global.InstalledBinaryName, runtime.GOOS, runtime.GOARCH)

// makeTarGz builds a gzipped tar holding files (name -> content).
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGitHub serves one release over the GitHub releases API shape, plus its
// assets, from an httptest server.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	signer *testSigner

	mu      sync.Mutex
	tag     string            // the release's tag
	bareTag bool              // the release is found only under its tag without "v"
	assets  map[string][]byte // asset name -> body
	status  map[string]int    // request path -> forced status
	body    map[string]string // request path -> forced body
	paths   []string          // every request path, in order
	short   bool              // the archive download ends early
}

func newFakeGitHub(t *testing.T, tag string, archive []byte) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, signer: newTestSigner(t), tag: tag, status: map[string]int{}, body: map[string]string{}}
	f.setArchive(archive)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// setArchive publishes archive with a checksum list signed by the signer.
func (f *fakeGitHub) setArchive(archive []byte) {
	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")
	f.assets = map[string][]byte{
		archiveName:        archive,
		checksumsAssetName: checksums,
		signatureAssetName: f.signer.sign(checksums, "test release"),
	}
}

func (f *fakeGitHub) release() GitHubRelease {
	rel := GitHubRelease{TagName: f.tag, Name: f.tag}
	for name := range f.assets {
		rel.Assets = append(rel.Assets, GitHubAsset{Name: name, BrowserDownloadURL: f.srv.URL + "/assets/" + name})
	}
	return rel
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	if st, ok := f.status[r.URL.Path]; ok {
		w.WriteHeader(st)
		if _, cerr := io.WriteString(w, f.body[r.URL.Path]); cerr != nil {
			f.t.Error(cerr)
		}
		return
	}
	if b, ok := f.body[r.URL.Path]; ok {
		if _, cerr := io.WriteString(w, b); cerr != nil {
			f.t.Error(cerr)
		}
		return
	}
	switch {
	case r.URL.Path == "/releases/latest":
		if cerr := json.NewEncoder(w).Encode(f.release()); cerr != nil {
			f.t.Error(cerr)
		}
	case strings.HasPrefix(r.URL.Path, "/releases/tags/"):
		want := f.tag
		if f.bareTag {
			want = strings.TrimPrefix(f.tag, "v")
		}
		if strings.TrimPrefix(r.URL.Path, "/releases/tags/") != want {
			http.NotFound(w, r)
			return
		}
		if cerr := json.NewEncoder(w).Encode(f.release()); cerr != nil {
			f.t.Error(cerr)
		}
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		b, ok := f.assets[strings.TrimPrefix(r.URL.Path, "/assets/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if f.short && strings.HasSuffix(r.URL.Path, archiveName) {
			// Promise more than is sent, so the client sees the body end early.
			w.Header().Set("Content-Length", strconv.Itoa(len(b)+100))
		}
		if _, cerr := w.Write(b); cerr != nil {
			f.t.Error(cerr)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// upgradeEnv is a fake machine for runUpgrade: the running binary (and its
// claw-auth companion) in a temp bin dir, no installed service unless set,
// and a recording service manager on which nothing is active.
type upgradeEnv struct {
	gh       *fakeGitHub
	binDir   string
	exe      string
	install  *install.ExistingInstall
	active   map[string]bool // joined command line -> succeeds
	commands []string
}

const newBinary = "#!/bin/sh\necho new claw\n"

func newUpgradeEnv(t *testing.T, tag string) *upgradeEnv {
	t.Helper()
	tmpRoot := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmpRoot); err == nil {
		tmpRoot = resolved
	}
	t.Setenv("TMPDIR", tmpRoot)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(global.EnvVarHome, "")
	binDir := filepath.Join(tmpRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &upgradeEnv{
		gh:     newFakeGitHub(t, tag, makeTarGz(t, map[string]string{"claw": newBinary, "claw-auth": "new auth"})),
		binDir: binDir,
		active: map[string]bool{},
	}
	e.exe = filepath.Join(e.binDir, "claw")
	if err := os.WriteFile(e.exe, []byte("old claw"), 0o755); err != nil {
		t.Fatal(err)
	}

	saved := []func(){}
	restore := func(f func()) { saved = append(saved, f) }
	oldBase, oldExe, oldDetect, oldKeys := apiBaseURL, executable, detectInstall, releaseKeys
	oldGOOS, oldEUID, oldUID, oldRun, oldOut := goos, geteuid, getuid, runCommand, commandOutput
	restore(func() {
		apiBaseURL, executable, detectInstall, releaseKeys = oldBase, oldExe, oldDetect, oldKeys
		goos, geteuid, getuid, runCommand, commandOutput = oldGOOS, oldEUID, oldUID, oldRun, oldOut
	})
	t.Cleanup(func() {
		for _, f := range saved {
			f()
		}
	})

	apiBaseURL = e.gh.srv.URL
	executable = func() (string, error) { return e.exe, nil }
	detectInstall = func(string) *install.ExistingInstall { return e.install }
	releaseKeys = func() []string { return []string{e.gh.signer.publicKeyLine()} }
	goos = "linux"
	geteuid = func() int { return 1000 }
	getuid = func() int { return 1000 }
	runCommand = func(name string, args ...string) error {
		line := strings.Join(append([]string{name}, args...), " ")
		e.commands = append(e.commands, line)
		if e.active[line] {
			return nil
		}
		return errors.New("exit status 3")
	}
	commandOutput = func(name string, args ...string) ([]byte, error) {
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	}
	return e
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Error(cerr)
		}
		done <- buf.String()
	}()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// withStdin runs fn with os.Stdin reading input.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if _, cerr := io.WriteString(w, input); cerr != nil {
			t.Error(cerr)
		}
		if cerr := w.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	os.Stdin = r
	defer func() {
		os.Stdin = old
		if cerr := r.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	fn()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const newerTag = "v99.0.0"

func TestRunUpgrade_CheckOnly(t *testing.T) {
	cur := app.SemVer()
	tests := []struct {
		tag  string
		want string
	}{
		{newerTag, "A newer version of " + app.ShortName() + " is available: v" + cur + " -> v99.0.0"},
		{"v0.0.0", "is running a newer version than latest release"},
		{"v" + cur, "is already up to date (v" + cur + ")"},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			e := newUpgradeEnv(t, tc.tag)
			var err error
			out := captureStdout(t, func() { err = runUpgrade(true, false, "", false) })
			if err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
			for _, p := range e.gh.requested() {
				if strings.HasPrefix(p, "/assets/") {
					t.Errorf("--check downloaded %s", p)
				}
			}
			if readFile(t, e.exe) != "old claw" {
				t.Error("--check replaced the binary")
			}
		})
	}
}

// TestRunUpgrade_UpToDate: without --force nothing is downloaded when the
// release is not newer.
func TestRunUpgrade_UpToDate(t *testing.T) {
	e := newUpgradeEnv(t, "v"+app.SemVer())
	var err error
	out := captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if !strings.Contains(out, "upgrade --force") {
		t.Errorf("output lacks the --force hint:\n%s", out)
	}
	if readFile(t, e.exe) != "old claw" {
		t.Error("an up-to-date binary was replaced")
	}
}

// TestRunUpgrade_InstallsVerifiedRelease: the signed archive replaces the
// running binary and its claw-auth companion, keeps it executable, removes
// the download directory and restarts the active user service.
func TestRunUpgrade_InstallsVerifiedRelease(t *testing.T) {
	e := newUpgradeEnv(t, newerTag)
	auth := filepath.Join(e.binDir, global.AuthHelperBinaryName)
	if err := os.WriteFile(auth, []byte("old auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.active["systemctl --user is-active --quiet "+upgradeServiceName] = true
	e.active["systemctl --user restart "+upgradeServiceName] = true

	var err error
	out := captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
	if err != nil {
		t.Fatalf("runUpgrade: %v\n%s", err, out)
	}
	if got := readFile(t, e.exe); got != newBinary {
		t.Errorf("binary = %q, want the release's", got)
	}
	if fi, serr := os.Stat(e.exe); serr != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %v (%v), want 0755", fi.Mode().Perm(), serr)
	}
	if got := readFile(t, auth); got != "new auth" {
		t.Errorf("claw-auth = %q, want the release's", got)
	}
	for _, want := range []string{
		"Signature and checksum verified.",
		"Updated companion tool: " + auth,
		"Successfully upgraded " + app.Name() + " to v99.0.0!",
		"User service restarted successfully.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	entries, rerr := os.ReadDir(os.Getenv("TMPDIR"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "claw-upgrade-") {
			t.Errorf("download directory %s left behind", ent.Name())
		}
	}
	leftovers, rerr := filepath.Glob(filepath.Join(e.binDir, "*.new.*"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary binaries left behind: %v", leftovers)
	}
}

// TestRunUpgrade_NoCompanionInstalled: a claw-auth in the archive is not
// installed where there was none.
func TestRunUpgrade_NoCompanionInstalled(t *testing.T) {
	e := newUpgradeEnv(t, newerTag)
	var err error
	out := captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.binDir, global.AuthHelperBinaryName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("claw-auth installed although none was there: %v", serr)
	}
	if strings.Contains(out, "Updated companion tool") {
		t.Errorf("companion reported updated:\n%s", out)
	}
}

// TestRunUpgrade_Confirmation: the upgrade asks first; anything but y/yes
// (or no answer) cancels without touching the binary.
func TestRunUpgrade_Confirmation(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		upgraded bool
	}{
		{"y\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"", false},
	} {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			e := newUpgradeEnv(t, newerTag)
			var err error
			var out string
			withStdin(t, tc.answer, func() {
				out = captureStdout(t, func() { err = runUpgrade(false, false, "", false) })
			})
			if err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			if !strings.Contains(out, "Do you want to proceed with the upgrade? [y/N]: ") {
				t.Errorf("no confirmation prompt:\n%s", out)
			}
			if got := readFile(t, e.exe) == newBinary; got != tc.upgraded {
				t.Errorf("upgraded = %v, want %v\n%s", got, tc.upgraded, out)
			}
			if !tc.upgraded && !strings.Contains(out, "Upgrade cancelled.") {
				t.Errorf("no cancel message:\n%s", out)
			}
		})
	}
}

// TestRunUpgrade_SpecificVersion: --version fetches that tag (adding the "v"),
// falls back to the bare tag when GitHub has no "v" tag, and installs it even
// when it is older than the running version.
func TestRunUpgrade_SpecificVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		bare          bool
	}{
		{"v tag", "0.0.1", false},
		{"v tag given with v", "v0.0.1", false},
		{"bare tag", "0.0.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUpgradeEnv(t, "v0.0.1")
			e.gh.bareTag = tc.bare
			var err error
			captureStdout(t, func() { err = runUpgrade(false, false, tc.version, true) })
			if err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			if readFile(t, e.exe) != newBinary {
				t.Error("the requested version was not installed")
			}
			paths := e.gh.requested()
			if paths[0] != "/releases/tags/v0.0.1" {
				t.Errorf("first request %q, want the v tag", paths[0])
			}
			if tc.bare && paths[1] != "/releases/tags/0.0.1" {
				t.Errorf("second request %q, want the bare tag", paths[1])
			}
		})
	}
}

// TestRunUpgrade_ForceReinstall: --force reinstalls the current version.
func TestRunUpgrade_ForceReinstall(t *testing.T) {
	e := newUpgradeEnv(t, "v"+app.SemVer())
	var err error
	captureStdout(t, func() { err = runUpgrade(false, true, "", true) })
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if readFile(t, e.exe) != newBinary {
		t.Error("--force did not reinstall")
	}
}

// TestRunUpgrade_ExistingInstall: when the running binary is a build in a
// temporary location the installed binary is upgraded instead, its data
// directory is exported and the service is shown in the summary.
func TestRunUpgrade_ExistingInstall(t *testing.T) {
	e := newUpgradeEnv(t, newerTag)
	installedDir := t.TempDir()
	installed := filepath.Join(installedDir, "claw")
	if err := os.WriteFile(installed, []byte("installed claw"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	for _, active := range []bool{true, false} {
		e.install = &install.ExistingInstall{
			BinaryPath: installed, AgencyHome: dataDir, ServicePath: "/etc/systemd/system/claw.service",
			ServiceType: "systemd-system", IsActive: active,
		}
		var err error
		out := captureStdout(t, func() { err = runUpgrade(false, true, "", true) })
		if err != nil {
			t.Fatalf("runUpgrade: %v", err)
		}
		state := "inactive"
		if active {
			state = "active"
		}
		for _, want := range []string{
			"Detected installed " + app.ShortName() + " at: " + installed,
			"Target Binary:   " + installed,
			"Data Directory:  " + dataDir,
			"Existing Service: /etc/systemd/system/claw.service (systemd-system, " + state + ")",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if readFile(t, installed) != newBinary {
			t.Error("the installed binary was not upgraded")
		}
		if os.Getenv("AGENCY_HOME") != dataDir || os.Getenv(global.EnvVarHome) != dataDir {
			t.Errorf("AGENCY_HOME = %q, want %q", os.Getenv("AGENCY_HOME"), dataDir)
		}
		if err := os.WriteFile(installed, []byte("installed claw"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if readFile(t, e.exe) != "old claw" {
		t.Error("the temporary build was replaced instead of the installed binary")
	}
}

func TestRunUpgrade_Errors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e *upgradeEnv)
		want  string
	}{
		{"release lookup fails", func(_ *testing.T, e *upgradeEnv) {
			e.gh.status["/releases/latest"] = http.StatusInternalServerError
			e.gh.body["/releases/latest"] = "server on fire"
		}, "GitHub API returned HTTP 500: server on fire"},
		{"release JSON garbled", func(_ *testing.T, e *upgradeEnv) {
			e.gh.body["/releases/latest"] = "{not json"
		}, "parsing release JSON"},
		{"release server down", func(_ *testing.T, e *upgradeEnv) {
			e.gh.srv.Close()
		}, "fetching release information"},
		{"bad API URL", func(_ *testing.T, _ *upgradeEnv) {
			apiBaseURL = "http://bad host"
		}, "fetching release information"},
		{"unsigned release", func(_ *testing.T, e *upgradeEnv) {
			delete(e.gh.assets, signatureAssetName)
		}, "refusing to install an unverifiable release"},
		{"no executable", func(_ *testing.T, _ *upgradeEnv) {
			executable = func() (string, error) { return "", errors.New("no /proc") }
		}, "locating current executable: no /proc"},
		{"no home directory", func(t *testing.T, _ *upgradeEnv) {
			t.Helper()
			t.Setenv("HOME", "")
		}, "determining home directory"},
		{"install directory not writable", func(t *testing.T, e *upgradeEnv) {
			t.Helper()
			e.exe = filepath.Join(t.TempDir(), "missing", "claw")
		}, "permission denied writing to"},
		{"archive download fails", func(_ *testing.T, e *upgradeEnv) {
			e.gh.status["/assets/"+archiveName] = http.StatusBadGateway
		}, "downloading " + archiveName + ": HTTP 502"},
		{"checksum list download fails", func(_ *testing.T, e *upgradeEnv) {
			e.gh.status["/assets/"+checksumsAssetName] = http.StatusNotFound
		}, "downloading checksums.txt: HTTP 404"},
		{"signature download fails", func(_ *testing.T, e *upgradeEnv) {
			e.gh.status["/assets/"+signatureAssetName] = http.StatusNotFound
		}, "downloading checksums.txt.minisig: HTTP 404"},
		{"signed by a foreign key", func(t *testing.T, _ *upgradeEnv) {
			t.Helper()
			stranger := newTestSigner(t)
			releaseKeys = func() []string { return []string{stranger.publicKeyLine()} }
		}, "release verification failed"},
		{"archive without claw", func(t *testing.T, e *upgradeEnv) {
			t.Helper()
			e.gh.setArchive(makeTarGz(t, map[string]string{"README": "hi"}))
		}, "extracting binary from archive"},
		{"no temp directory", func(t *testing.T, _ *upgradeEnv) {
			t.Helper()
			t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
		}, "creating temp directory"},
		{"archive cut short", func(t *testing.T, e *upgradeEnv) {
			t.Helper()
			e.gh.short = true
		}, "downloading " + archiveName},
		{"target is a directory", func(t *testing.T, e *upgradeEnv) {
			t.Helper()
			e.exe = filepath.Join(t.TempDir(), "claw")
			if err := os.Mkdir(e.exe, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "replacing binary at"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newUpgradeEnv(t, newerTag)
			tc.setup(t, e)
			var err error
			captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runUpgrade = %v, want it to contain %q", err, tc.want)
			}
			if fi, serr := os.Stat(e.exe); serr == nil && !fi.IsDir() && readFile(t, e.exe) != "old claw" {
				t.Error("binary replaced by a failed upgrade")
			}
			if len(e.commands) != 0 {
				t.Errorf("failed upgrade ran %v", e.commands)
			}
		})
	}
}

// TestFetchRelease_SpecificVersionMissing: a tag found neither with nor
// without "v" reports GitHub's 404.
func TestFetchRelease_SpecificVersionMissing(t *testing.T) {
	newUpgradeEnv(t, newerTag)
	if _, err := fetchRelease("1.2.3"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("fetchRelease = %v, want a 404", err)
	}
}

func TestDownloadHelpers_Errors(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	dst := filepath.Join(t.TempDir(), "out")
	for _, url := range []string{"http://bad host/x", deadURL + "/x"} {
		if err := downloadFile(url, dst); err == nil {
			t.Errorf("downloadFile(%q) succeeded", url)
		}
		if _, err := downloadBytes(url); err == nil {
			t.Errorf("downloadBytes(%q) succeeded", url)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, werr := io.WriteString(w, "data"); werr != nil {
			t.Error(werr)
		}
	}))
	defer srv.Close()
	if err := downloadFile(srv.URL, filepath.Join(t.TempDir(), "missing", "out")); err == nil {
		t.Error("downloadFile into a missing directory succeeded")
	}
	if err := downloadFile(srv.URL, dst); err != nil || readFile(t, dst) != "data" {
		t.Errorf("downloadFile = %v", err)
	}
}

func TestAtomicReplace_MissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := atomicReplace(filepath.Join(dir, "nope"), filepath.Join(dir, "claw")); err == nil {
		t.Fatal("atomicReplace of a missing source succeeded")
	}
	if err := atomicReplace(filepath.Join(dir, "nope"), filepath.Join(dir, "missing", "claw")); err == nil {
		t.Fatal("atomicReplace into a missing directory succeeded")
	}
}

// TestAtomicReplace_NewFile: a target that does not exist yet is created
// executable.
func TestAtomicReplace_NewFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "claw")
	if err := atomicReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o755 || readFile(t, dst) != "bin" {
		t.Fatalf("dst = %v, %v", fi, err)
	}
}

func TestIsBuildOrTempDir(t *testing.T) {
	for p, want := range map[string]bool{
		"/home/alice/src/build/claw": true,
		"/tmp/claw":                  true,
		"/var/temp/claw":             true,
		"/x/claw-upgrade-123/claw":   true,
		"/usr/local/bin/claw":        false,
		"/opt/agency/bin/claw":       false,
		"/home/alice/buildings/claw": false,
	} {
		if got := isBuildOrTempDir(p); got != want {
			t.Errorf("isBuildOrTempDir(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestExtractBinariesFromTarGz_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := extractBinariesFromTarGz(filepath.Join(dir, "missing.tar.gz"), "", ""); err == nil {
		t.Error("missing archive accepted")
	}
	notGz := filepath.Join(dir, "plain")
	if err := os.WriteFile(notGz, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinariesFromTarGz(notGz, "", ""); err == nil {
		t.Error("non-gzip archive accepted")
	}
	var badTar bytes.Buffer
	gz := gzip.NewWriter(&badTar)
	if _, cerr := gz.Write([]byte("this is not a tar stream, but long enough to be read as a header block ....")); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	badTarPath := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(badTarPath, badTar.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinariesFromTarGz(badTarPath, filepath.Join(dir, "c"), filepath.Join(dir, "a")); err == nil {
		t.Error("corrupt tar accepted")
	}
	good := filepath.Join(dir, "good.tar.gz")
	if err := os.WriteFile(good, makeTarGz(t, map[string]string{"claw": "c", "claw-auth": "a"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinariesFromTarGz(good, filepath.Join(dir, "missing", "c"), filepath.Join(dir, "a")); err == nil {
		t.Error("unwritable claw destination accepted")
	}
	onlyAuth := filepath.Join(dir, "auth.tar.gz")
	if err := os.WriteFile(onlyAuth, makeTarGz(t, map[string]string{"claw-auth": "a"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinariesFromTarGz(onlyAuth, filepath.Join(dir, "c"), filepath.Join(dir, "missing", "a")); err == nil {
		t.Error("unwritable claw-auth destination accepted")
	}
	// A truncated member fails while copying it out.
	full := makeTarGz(t, map[string]string{"claw": strings.Repeat("x", 4096)})
	raw, err := gzip.NewReader(bytes.NewReader(full))
	if err != nil {
		t.Fatal(err)
	}
	plain, rerr := io.ReadAll(raw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var trunc bytes.Buffer
	gz = gzip.NewWriter(&trunc)
	if _, cerr := gz.Write(plain[:512+100]); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	truncPath := filepath.Join(dir, "trunc.tar.gz")
	if ierr := os.WriteFile(truncPath, trunc.Bytes(), 0o600); ierr != nil {
		t.Fatal(ierr)
	}
	if _, ierr := extractBinariesFromTarGz(truncPath, filepath.Join(dir, "c2"), filepath.Join(dir, "a2")); ierr == nil {
		t.Error("truncated claw member accepted")
	}
	authFull := makeTarGz(t, map[string]string{"claw-auth": strings.Repeat("x", 4096)})
	raw, err = gzip.NewReader(bytes.NewReader(authFull))
	if err != nil {
		t.Fatal(err)
	}
	plain, err = io.ReadAll(raw)
	if err != nil {
		t.Fatal(err)
	}
	trunc.Reset()
	gz = gzip.NewWriter(&trunc)
	if _, cerr := gz.Write(plain[:512+100]); cerr != nil {
		t.Error(cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		t.Error(cerr)
	}
	if err := os.WriteFile(truncPath, trunc.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinariesFromTarGz(truncPath, filepath.Join(dir, "c3"), filepath.Join(dir, "a3")); err == nil {
		t.Error("truncated claw-auth member accepted")
	}
}

func TestComputeSHA256_Missing(t *testing.T) {
	if _, err := computeSHA256(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("computeSHA256 of a missing file succeeded")
	}
}

// TestRestartActiveService covers which service is restarted, on each
// service manager, given what is active.
func TestRestartActiveService(t *testing.T) {
	const svc = upgradeServiceName
	const label = global.LaunchdLabel
	tests := []struct {
		name     string
		goos     string
		euid     int
		active   []string
		launchd  string
		wantCmds []string
		wantOut  string
	}{
		{
			name:     "user service",
			goos:     "linux",
			active:   []string{"systemctl --user is-active --quiet " + svc, "systemctl --user restart " + svc},
			wantCmds: []string{"systemctl --user is-active --quiet " + svc, "systemctl --user restart " + svc},
			wantOut:  "User service restarted successfully.",
		},
		{
			name:   "user restart fails, system service as root",
			goos:   "linux",
			euid:   0,
			active: []string{"systemctl --user is-active --quiet " + svc, "systemctl is-active --quiet " + svc, "systemctl restart " + svc},
			wantCmds: []string{
				"systemctl --user is-active --quiet " + svc, "systemctl --user restart " + svc,
				"systemctl is-active --quiet " + svc, "systemctl restart " + svc,
			},
			wantOut: "System service restarted successfully.",
		},
		{
			name:     "system service, not root",
			goos:     "linux",
			euid:     1000,
			active:   []string{"systemctl is-active --quiet " + svc},
			wantCmds: []string{"systemctl --user is-active --quiet " + svc, "systemctl is-active --quiet " + svc},
			wantOut:  "Run `sudo systemctl restart " + svc + "` to apply the update.",
		},
		{
			name:     "system restart fails",
			goos:     "linux",
			euid:     0,
			active:   []string{"systemctl is-active --quiet " + svc},
			wantCmds: []string{"systemctl --user is-active --quiet " + svc, "systemctl is-active --quiet " + svc, "systemctl restart " + svc},
			wantOut:  "Restarting systemd system service",
		},
		{
			name:     "nothing active",
			goos:     "linux",
			wantCmds: []string{"systemctl --user is-active --quiet " + svc, "systemctl is-active --quiet " + svc},
		},
		{
			name:     "launchd agent",
			goos:     "darwin",
			launchd:  "123\t0\t" + label + "\n",
			active:   []string{"launchctl kickstart -k gui/501/" + label},
			wantCmds: []string{"launchctl kickstart -k gui/501/" + label},
			wantOut:  "Launchd service restarted successfully.",
		},
		{
			name:     "launchd daemon",
			goos:     "darwin",
			launchd:  label + "\n",
			active:   []string{"launchctl kickstart -k system/" + label},
			wantCmds: []string{"launchctl kickstart -k gui/501/" + label, "launchctl kickstart -k system/" + label},
			wantOut:  "Restarting launchd service",
		},
		{
			name:     "launchd restart fails",
			goos:     "darwin",
			launchd:  label + "\n",
			wantCmds: []string{"launchctl kickstart -k gui/501/" + label, "launchctl kickstart -k system/" + label},
			wantOut:  "Warning: could not restart launchd service " + label,
		},
		{
			name:    "launchd not loaded",
			goos:    "darwin",
			launchd: "com.example.other\n",
		},
		{
			name: "other system",
			goos: "freebsd",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newUpgradeEnv(t, newerTag)
			goos = tc.goos
			geteuid = func() int { return tc.euid }
			getuid = func() int { return 501 }
			for _, c := range tc.active {
				e.active[c] = true
			}
			commandOutput = func(name string, args ...string) ([]byte, error) {
				if line := strings.Join(append([]string{name}, args...), " "); line != "launchctl list" {
					t.Errorf("unexpected %s", line)
				}
				return []byte(tc.launchd), nil
			}
			out := captureStdout(t, restartActiveService)
			if strings.Join(e.commands, "|") != strings.Join(tc.wantCmds, "|") {
				t.Errorf("commands = %q, want %q", e.commands, tc.wantCmds)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			if tc.wantOut == "" && out != "" {
				t.Errorf("unexpected output:\n%s", out)
			}
		})
	}
}

func TestNewUpgradeCommand(t *testing.T) {
	e := newUpgradeEnv(t, newerTag)
	cmd := NewUpgradeCommand()
	if cmd.Name() != "upgrade" || len(cmd.Aliases) != 1 || cmd.Aliases[0] != "update" {
		t.Errorf("command %q aliases %v", cmd.Name(), cmd.Aliases)
	}
	cmd.SetArgs([]string{"--check"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("upgrade --check: %v", err)
	}
	if !strings.Contains(out, "A newer version") || readFile(t, e.exe) != "old claw" {
		t.Errorf("--check output %q", out)
	}
	cmd = NewUpgradeCommand()
	cmd.SetArgs([]string{"-y", "--version", "99.0.0"})
	out = captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("upgrade -y --version: %v\n%s", err, out)
	}
	if readFile(t, e.exe) != newBinary {
		t.Error("upgrade -y did not install")
	}
}

// TestRunUpgrade_PackageManagedLinux: a binary in /usr/bin on Linux is owned by
// dpkg/apt, so `claw upgrade` refuses to overwrite it and directs the operator
// to apt, while `claw upgrade --check` still checks GitHub and prints the apt
// command.
func TestRunUpgrade_PackageManagedLinux(t *testing.T) {
	t.Run("direct /usr/bin/claw refuses before hitting GitHub", func(t *testing.T) {
		e := newUpgradeEnv(t, newerTag)
		executable = func() (string, error) { return "/usr/bin/claw", nil }

		var err error
		captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
		if err == nil || !strings.Contains(err.Error(), "sudo apt update && sudo apt install --only-upgrade "+global.DebPackageName) {
			t.Fatalf("err = %v, want apt upgrade hint", err)
		}
		if got := e.gh.requested(); len(got) != 0 {
			t.Errorf("expected no GitHub requests, got %v", got)
		}
	})

	t.Run("--check prints apt hint", func(t *testing.T) {
		_ = newUpgradeEnv(t, newerTag)
		executable = func() (string, error) { return "/usr/bin/claw", nil }

		var err error
		out := captureStdout(t, func() { err = runUpgrade(true, false, "", false) })
		if err != nil {
			t.Fatalf("upgrade --check: %v", err)
		}
		want := "sudo apt update && sudo apt install --only-upgrade " + global.DebPackageName
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	})

	t.Run("build binary targeting detected /usr/bin/claw refuses", func(t *testing.T) {
		e := newUpgradeEnv(t, newerTag)
		buildExe := filepath.Join(t.TempDir(), "build", "claw")
		if err := os.MkdirAll(filepath.Dir(buildExe), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(buildExe, []byte("build"), 0o755); err != nil {
			t.Fatal(err)
		}
		executable = func() (string, error) { return buildExe, nil }
		e.install = &install.ExistingInstall{BinaryPath: "/usr/bin/claw"}

		var err error
		captureStdout(t, func() { err = runUpgrade(false, false, "", true) })
		if err == nil || !strings.Contains(err.Error(), "sudo apt update && sudo apt install --only-upgrade "+global.DebPackageName) {
			t.Fatalf("err = %v, want apt upgrade hint", err)
		}
	})
}
