// Agency
// License: MIT

package utils

import (
	"archive/zip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type errCloser struct{ closed bool }

func (c *errCloser) Close() error {
	c.closed = true
	return errors.New("boom")
}

func TestCloseQuietly(t *testing.T) {
	c := &errCloser{}
	CloseQuietly(c) // an error is logged, not returned or panicked on
	if !c.closed {
		t.Fatal("Close was not called")
	}
}

func TestValidateSkillIdentifier(t *testing.T) {
	for _, ok := range []string{"weather", " github-pr ", "a.b"} {
		if err := ValidateSkillIdentifier(ok); err != nil {
			t.Errorf("ValidateSkillIdentifier(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "a/b", `a\b`, "..", "a..b"} {
		if err := ValidateSkillIdentifier(bad); err == nil {
			t.Errorf("ValidateSkillIdentifier(%q) = nil, want error", bad)
		}
	}
}

func TestDerefStr(t *testing.T) {
	s := "alice"
	if got := DerefStr(&s, "bob"); got != "alice" {
		t.Errorf("DerefStr(&alice) = %q", got)
	}
	if got := DerefStr(nil, "bob"); got != "bob" {
		t.Errorf("DerefStr(nil) = %q, want fallback", got)
	}
}

func TestSetDisableTruncation(t *testing.T) {
	SetDisableTruncation(true)
	t.Cleanup(func() { SetDisableTruncation(false) })
	long := strings.Repeat("x", 50)
	if got := Truncate(long, 5); got != long {
		t.Errorf("Truncate with truncation disabled = %q, want the full string", got)
	}
	SetDisableTruncation(false)
	if got := Truncate(long, 5); got != "xx..." {
		t.Errorf("Truncate re-enabled = %q, want xx...", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := map[string]string{
		"photo.jpg":        "photo.jpg",
		"../../etc/passwd": "passwd",
		"a..b.txt":         "ab.txt",
		`dir\evil.txt`:     "dir_evil.txt",
	}
	for in, want := range tests {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisteredDomain(t *testing.T) {
	tests := map[string]string{
		"files.slack.com": "slack.com",
		"a.b.example.co":  "example.co",
		"example.com":     "example.com",
		"localhost":       "localhost",
		// An IP address is its own site.
		"10.0.0.1":    "10.0.0.1",
		"192.168.0.1": "192.168.0.1",
		"2001:db8::1": "2001:db8::1",
		"::1":         "::1",
	}
	for in, want := range tests {
		if got := registeredDomain(in); got != want {
			t.Errorf("registeredDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// useMediaDir points media staging at a fresh temp dir for one test.
func useMediaDir(t *testing.T, dirMode, fileMode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "media")
	SetMediaStagingDir(dir, dirMode, fileMode)
	t.Cleanup(func() { SetMediaStagingDir("", 0, 0) })
	return dir
}

func TestMediaStagingDirAndModes(t *testing.T) {
	SetMediaStagingDir("", 0, 0)
	if d, f := MediaModes(); d != 0o700 || f != 0o600 {
		t.Errorf("zero modes = %04o/%04o, want 0700/0600", d, f)
	}
	if got, want := MediaTempDir(), filepath.Join(os.TempDir(), "claw_media_"); !strings.HasPrefix(got, want) {
		t.Errorf("fallback MediaTempDir = %q, want prefix %q", got, want)
	}
	dir := useMediaDir(t, 0o750, 0o640)
	if got := MediaTempDir(); got != dir {
		t.Errorf("MediaTempDir = %q, want %q", got, dir)
	}
	if d, f := MediaModes(); d != 0o750 || f != 0o640 {
		t.Errorf("modes = %04o/%04o, want 0750/0640", d, f)
	}
}

func TestDownloadFile_Success(t *testing.T) {
	dir := useMediaDir(t, 0o700, 0o600)
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/file", http.StatusFound)
			return
		}
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		mustWrite(t, w, []byte("PNGDATA"))
	}))
	defer srv.Close()

	path := DownloadFile(srv.URL+"/start", "../pic.png", DownloadOptions{
		ExtraHeaders: map[string]string{"Authorization": "Bearer t"},
		LogURL:       "https://redacted/",
	})
	if path == "" {
		t.Fatal("DownloadFile returned empty path")
	}
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, "_pic.png") {
		t.Errorf("path = %q, want a uuid_pic.png file in %q", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("file content = %q, %v", data, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %04o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", di.Mode().Perm())
	}
	// A same-host redirect keeps the Authorization header.
	if got := gotAuth.Load(); got != "Bearer t" {
		t.Errorf("Authorization after redirect = %q, want Bearer t", got)
	}
}

func TestDownloadFile_Failures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			http.NotFound(w, r)
		case "/html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			mustWrite(t, w, []byte("<html>login</html>"))
		case "/slack":
			http.Redirect(w, r, "/slack.com/login?redir=x", http.StatusFound)
		case "/slack.com/login":
			w.Header().Set("Content-Type", "text/html")
			mustWrite(t, w, []byte("<html>sign in</html>"))
		case "/short":
			// Promise more than is sent so the copy fails mid-body.
			w.Header().Set("Content-Length", "100")
			mustWrite(t, w, []byte("partial"))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer srv.Close()

	tests := []struct {
		name string
		url  string
		opts DownloadOptions
	}{
		{"non-200", srv.URL + "/404", DownloadOptions{}},
		{"html page", srv.URL + "/html", DownloadOptions{}},
		{"slack login page", srv.URL + "/slack", DownloadOptions{}},
		{"truncated body", srv.URL + "/short", DownloadOptions{}},
		{"redirect loop", srv.URL + "/loop", DownloadOptions{}},
		{"bad url", "http://[::1", DownloadOptions{}},
		{"bad proxy", srv.URL + "/404", DownloadOptions{ProxyURL: "://nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := useMediaDir(t, 0, 0)
			if got := DownloadFile(tt.url, "f.bin", tt.opts); got != "" {
				t.Fatalf("DownloadFile = %q, want empty", got)
			}
			// Nothing is left behind in the staging dir.
			entries := dirEntries(t, dir)
			if len(entries) != 0 {
				t.Errorf("staging dir holds %d entries, want none", len(entries))
			}
		})
	}
}

func TestDownloadFile_ViaProxy(t *testing.T) {
	useMediaDir(t, 0, 0)
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Store(r.URL.Host == "files.example.invalid")
		mustWrite(t, w, []byte("via proxy"))
	}))
	defer proxy.Close()

	path := DownloadFile("http://files.example.invalid/a.txt", "a.txt", DownloadOptions{ProxyURL: proxy.URL})
	if path == "" {
		t.Fatal("download through proxy failed")
	}
	if !proxied.Load() {
		t.Error("request did not go through the proxy")
	}
}

func TestDownloadFile_MediaDirUnderFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	SetMediaStagingDir(filepath.Join(blocker, "media"), 0, 0)
	t.Cleanup(func() { SetMediaStagingDir("", 0, 0) })
	if got := DownloadFile("http://127.0.0.1:1/x", "x", DownloadOptions{}); got != "" {
		t.Fatalf("DownloadFile = %q, want empty when the media dir cannot be made", got)
	}
}

func TestDownloadFileSimple(t *testing.T) {
	useMediaDir(t, 0, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustWrite(t, w, []byte("ok"))
	}))
	defer srv.Close()
	if path := DownloadFileSimple(srv.URL, "ok.txt"); path == "" {
		t.Fatal("DownloadFileSimple failed")
	}
}

func TestMkdirExact_ExistingFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkdirExact(f, 0o700); !errors.Is(err, os.ErrExist) {
		t.Fatalf("mkdirExact on a file = %v, want ErrExist", err)
	}
}

func TestDownloadToFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			mustWrite(t, w, []byte("0123456789"))
		case "/err":
			http.Error(w, "nope", http.StatusTeapot)
		case "/short":
			w.Header().Set("Content-Length", "100")
			mustWrite(t, w, []byte("partial"))
		}
	}))
	defer srv.Close()

	get := func(path string, maxBytes int64) (string, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return DownloadToFile(context.Background(), srv.Client(), req, maxBytes)
	}

	for _, maxBytes := range []int64{0, 10} {
		p, err := get("/ok", maxBytes)
		if err != nil {
			t.Fatalf("max %d: %v", maxBytes, err)
		}
		data, err := os.ReadFile(p)
		if err != nil || string(data) != "0123456789" {
			t.Errorf("max %d: content %q, %v", maxBytes, data, err)
		}
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
	}

	if _, err := get("/ok", 9); err == nil || !strings.Contains(err.Error(), "download too large") {
		t.Errorf("over limit err = %v", err)
	}
	if _, err := get("/err", 0); err == nil || !strings.Contains(err.Error(), "HTTP 418: nope") {
		t.Errorf("status err = %v", err)
	}
	if _, err := get("/short", 0); err == nil || !strings.Contains(err.Error(), "download write failed") {
		t.Errorf("short body err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DownloadToFile(ctx, srv.Client(), req, 0); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("cancelled err = %v", err)
	}

	// Every failure removed its temp file.
	entries := dirEntries(t, os.Getenv("TMPDIR"))
	if len(entries) != 0 {
		t.Errorf("TMPDIR holds %d leftover entries", len(entries))
	}

	// A temp dir that cannot be written fails before any file exists.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := get("/ok", 0); err == nil || !strings.Contains(err.Error(), "failed to create temp file") {
		t.Errorf("bad TMPDIR err = %v", err)
	}
}

func TestRemoveTempFile(t *testing.T) {
	removeTempFile(filepath.Join(t.TempDir(), "absent")) // missing is not an error
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	removeTempFile(dir) // a non-empty dir cannot be removed; it is warned, not fatal
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir vanished: %v", err)
	}
}

// writeZip builds a zip at path with the given headers and contents.
func writeZip(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

type zipEntry struct {
	name string
	mode os.FileMode
	body string
}

func TestExtractZipFile_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		entries []zipEntry
		want    string
	}{
		{"traversal", []zipEntry{{"../evil", 0o644, "x"}}, "unsafe path"},
		{"absolute", []zipEntry{{"/etc/evil", 0o644, "x"}}, "unsafe path"},
		{"symlink", []zipEntry{{"link", os.ModeSymlink | 0o777, "/etc/passwd"}}, "symlinks are not allowed"},
		{"too large", []zipEntry{{"big", 0o644, strings.Repeat("a", 5*1024*1024+1)}}, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			zp := filepath.Join(tmp, "a.zip")
			writeZip(t, zp, tt.entries)
			err := ExtractZipFile(zp, filepath.Join(tmp, "out"), 0o700, 0o600)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExtractZipFile_Errors(t *testing.T) {
	tmp := t.TempDir()
	if err := ExtractZipFile(filepath.Join(tmp, "missing.zip"), tmp, 0o700, 0o600); err == nil || !strings.Contains(err.Error(), "invalid ZIP") {
		t.Errorf("missing zip err = %v", err)
	}

	zp := filepath.Join(tmp, "ok.zip")
	writeZip(t, zp, []zipEntry{{"a/b.txt", 0o644, "hi"}})

	// Target under a regular file.
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZipFile(zp, filepath.Join(blocker, "out"), 0o700, 0o600); err == nil || !strings.Contains(err.Error(), "failed to create target dir") {
		t.Errorf("target under file err = %v", err)
	}

	// The entry's parent already exists as a file.
	out := filepath.Join(tmp, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZipFile(zp, out, 0o700, 0o600); err == nil {
		t.Error("parent is a file: want error")
	}

	// A directory entry whose path exists as a file.
	zd := filepath.Join(tmp, "dir.zip")
	writeZip(t, zd, []zipEntry{{"a/", os.ModeDir | 0o755, ""}})
	if err := ExtractZipFile(zd, out, 0o700, 0o600); err == nil {
		t.Error("dir entry over a file: want error")
	}

	// The destination file exists as a directory.
	out2 := filepath.Join(tmp, "out2")
	if err := os.MkdirAll(filepath.Join(out2, "a", "b.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZipFile(zp, out2, 0o700, 0o600); err == nil || !strings.Contains(err.Error(), "failed to create file") {
		t.Errorf("dest is a dir err = %v", err)
	}
}

func TestExtractZipFile_CorruptEntry(t *testing.T) {
	tmp := t.TempDir()
	zp := filepath.Join(tmp, "c.zip")
	writeZip(t, zp, []zipEntry{{"c.txt", 0o644, "hello world, this is data"}})
	data, err := os.ReadFile(zp)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte of the stored body so the CRC check fails on read.
	i := strings.Index(string(data), "hello")
	data[i] ^= 0xff
	if err = os.WriteFile(zp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out")
	err = ExtractZipFile(zp, out, 0o700, 0o600)
	if err == nil || !strings.Contains(err.Error(), "failed to extract") {
		t.Fatalf("err = %v, want extract failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "c.txt")); !os.IsNotExist(statErr) {
		t.Errorf("partial file left behind: %v", statErr)
	}
}

func TestCreateHTTPClient_MissingHost(t *testing.T) {
	if _, err := CreateHTTPClient("http://", 0); err == nil || !strings.Contains(err.Error(), "missing host") {
		t.Fatalf("err = %v, want missing host", err)
	}
}

func TestDoRequestWithRetry_NonRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DoRequestWithRetry(srv.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	if resp.StatusCode != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls, want 404 after 1", resp.StatusCode, calls.Load())
	}
}

func TestExtractZipFile_UnsupportedMethod(t *testing.T) {
	tmp := t.TempDir()
	zp := filepath.Join(tmp, "m.zip")
	zf, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "x.txt", Method: 99, CompressedSize64: 2, UncompressedSize64: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = zf.Close(); err != nil {
		t.Fatal(err)
	}
	err = ExtractZipFile(zp, filepath.Join(tmp, "out"), 0o700, 0o600)
	if err == nil || !strings.Contains(err.Error(), "failed to open zip entry") {
		t.Fatalf("err = %v, want open failure", err)
	}
}

func TestDownloadFile_ReadOnlyMediaDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := useMediaDir(t, 0, 0)
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustWrite(t, w, []byte("ok"))
	}))
	defer srv.Close()
	if got := DownloadFile(srv.URL, "f", DownloadOptions{}); got != "" {
		t.Fatalf("DownloadFile = %q, want empty when the file cannot be created", got)
	}
}

func TestMkdirExact_ReadOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := mkdirExact(filepath.Join(parent, "child"), 0o700); err == nil {
		t.Fatal("mkdirExact under a read-only parent: want error")
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

type staticTransport struct{ resp *http.Response }

func (s staticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.resp.Request = r
	return s.resp, nil
}

// An error status whose body cannot be read still reports the status.
func TestDownloadToFile_ErrorBodyUnreadable(t *testing.T) {
	client := &http.Client{Transport: staticTransport{&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       failingBody{},
		Header:     http.Header{},
	}}}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DownloadToFile(context.Background(), client, req, 0)
	if err == nil || err.Error() != "HTTP 502: " {
		t.Fatalf("err = %v, want HTTP 502 with no body", err)
	}
}

// mustWrite writes b from a test handler, reporting a failure on t.
func mustWrite(t *testing.T, w http.ResponseWriter, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// dirEntries lists dir, failing the test when it cannot be read.
func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A redirect between two IP addresses never carries the Authorization header,
// even when their last two octets match (127.0.0.1 and 127.1.0.1).
func TestDownloadFile_RedirectBetweenIPsDropsAuthorization(t *testing.T) {
	useMediaDir(t, 0, 0)
	lnB, err := net.Listen("tcp", "127.1.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on 127.1.0.1: %v", err)
	}
	var gotAuth atomic.Value
	b := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		mustWrite(t, w, []byte("data"))
	}))
	b.Listener = lnB
	b.Start()
	defer b.Close()
	a := httptest.NewServer(http.RedirectHandler(b.URL+"/file", http.StatusFound))
	defer a.Close()

	if path := DownloadFile(a.URL, "f.bin", DownloadOptions{ExtraHeaders: map[string]string{"Authorization": "Bearer t"}}); path == "" {
		t.Fatal("download failed")
	}
	if got := gotAuth.Load(); got != "" {
		t.Errorf("Authorization at the second IP = %q, want none", got)
	}
}
