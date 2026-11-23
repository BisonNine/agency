package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- loader ---

func TestLoadSkill_WorkspaceThenGlobal(t *testing.T) {
	ws := t.TempDir()
	global := t.TempDir()
	createSkillDir(t, filepath.Join(ws, "skills"), "alpha", "alpha", "workspace alpha")
	createSkillDir(t, global, "alpha", "alpha", "global alpha")
	createSkillDir(t, global, "beta", "beta", "global beta")

	sl := NewSkillsLoader(ws, global)
	body, ok := sl.LoadSkill("alpha")
	require.True(t, ok)
	assert.Equal(t, "# alpha", body, "frontmatter is stripped and the workspace copy wins")

	body, ok = sl.LoadSkill("beta")
	require.True(t, ok)
	assert.Equal(t, "# beta", body)

	_, ok = sl.LoadSkill("missing")
	assert.False(t, ok)

	// No roots at all.
	_, ok = NewSkillsLoader("", "").LoadSkill("alpha")
	assert.False(t, ok)
}

func TestLoadSkillsForContext(t *testing.T) {
	global := t.TempDir()
	createSkillDir(t, global, "alpha", "alpha", "a")
	createSkillDir(t, global, "beta", "beta", "b")
	sl := NewSkillsLoader("", global)

	assert.Empty(t, sl.LoadSkillsForContext(nil))
	got := sl.LoadSkillsForContext([]string{"alpha", "missing", "beta"})
	assert.Equal(t, "### Skill: alpha\n\n# alpha\n\n---\n\n### Skill: beta\n\n# beta", got)
}

func TestBuildSkillsSummary(t *testing.T) {
	global := t.TempDir()
	createSkillDir(t, global, "alpha", "alpha", "uses <tags> & more")
	sl := NewSkillsLoader("", global)

	got := sl.BuildSkillsSummary()
	path := filepath.Join(global, "alpha", "SKILL.md")
	want := strings.Join([]string{
		"<skills>",
		"  <skill>",
		"    <name>alpha</name>",
		"    <description>uses &lt;tags&gt; &amp; more</description>",
		"    <location>" + path + "</location>",
		"    <source>global</source>",
		"  </skill>",
		"</skills>",
	}, "\n")
	assert.Equal(t, want, got)

	assert.Empty(t, NewSkillsLoader("", t.TempDir()).BuildSkillsSummary())
}

func TestGetSkillMetadata_JSONFrontmatterAndUnreadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-skill")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.WriteFile(p, []byte("---\n{\"name\":\"json-name\",\"description\":\"json desc\"}\n---\nbody"), 0o644))

	sl := NewSkillsLoader("", "")
	meta := sl.getSkillMetadata(p)
	require.NotNil(t, meta)
	assert.Equal(t, "json-name", meta.Name)
	assert.Equal(t, "json desc", meta.Description)

	// Empty JSON values keep the markdown/dir defaults.
	require.NoError(t, os.WriteFile(p, []byte("---\n{}\n---\nA paragraph."), 0o644))
	meta = sl.getSkillMetadata(p)
	assert.Equal(t, "my-skill", meta.Name)
	assert.Equal(t, "A paragraph.", meta.Description)

	// Invalid YAML falls back to the markdown metadata.
	require.NoError(t, os.WriteFile(p, []byte("---\nname: [unclosed\n---\nText here."), 0o644))
	meta = sl.getSkillMetadata(p)
	assert.Equal(t, "my-skill", meta.Name)
	assert.Equal(t, "Text here.", meta.Description)

	assert.Nil(t, sl.getSkillMetadata(filepath.Join(dir, "missing.md")))
}

func TestExtractMarkdownMetadata_CodeAndBreaks(t *testing.T) {
	title, desc := extractMarkdownMetadata("# my-tool\n\nRun `cmd` now\nand later.")
	assert.Equal(t, "my-tool", title)
	assert.Equal(t, "Run cmd now and later.", desc)

	// Description before the title: both are still found.
	title, desc = extractMarkdownMetadata("First para.\n\n# late-title\n")
	assert.Equal(t, "late-title", title)
	assert.Equal(t, "First para.", desc)
}

// --- search cache ---

func TestNewSearchCache_Defaults(t *testing.T) {
	sc := NewSearchCache(0, 0)
	assert.Equal(t, 50, sc.maxEntries)
	assert.Equal(t, 5*time.Minute, sc.ttl)
}

func TestSearchCache_PutUpdatesAndEvictsExpired(t *testing.T) {
	sc := NewSearchCache(10, time.Hour)
	sc.Put("weather", []SearchResult{{Slug: "a"}})
	sc.Put("weather", []SearchResult{{Slug: "b"}})
	got, ok := sc.Get("weather")
	require.True(t, ok)
	assert.Equal(t, "b", got[0].Slug, "a second Put replaces the results")
	assert.Equal(t, 1, sc.Len())

	sc.Put("   ", []SearchResult{{Slug: "x"}})
	assert.Equal(t, 1, sc.Len(), "an empty query is not stored")

	// Age the entry past its TTL; the next Put drops it.
	sc.mu.Lock()
	sc.entries["weather"].createdAt = time.Now().Add(-2 * time.Hour)
	sc.mu.Unlock()
	sc.Put("github", nil)
	_, ok = sc.Get("weather")
	assert.False(t, ok)
	got, ok = sc.Get("github")
	assert.True(t, ok)
	assert.Nil(t, got, "nil results stay nil")
}

// --- registry manager ---

func TestNewRegistryManagerFromConfig(t *testing.T) {
	rm := NewRegistryManagerFromConfig(RegistryConfig{MaxConcurrentSearches: 7})
	assert.Equal(t, 7, rm.maxConcurrent)
	assert.Nil(t, rm.GetRegistry("clawhub"))

	rm = NewRegistryManagerFromConfig(RegistryConfig{ClawHub: ClawHubConfig{Enabled: true}})
	assert.Equal(t, defaultMaxConcurrentSearches, rm.maxConcurrent)
	assert.NotNil(t, rm.GetRegistry("clawhub"))
}

// --- ClawHub ---

func TestNewClawHubRegistry_CustomConfig(t *testing.T) {
	r := NewClawHubRegistry(ClawHubConfig{
		BaseURL: "http://hub", SearchPath: "/s", SkillsPath: "/k", DownloadPath: "/d",
		Timeout: 3, MaxZipSize: 10, MaxResponseSize: 20,
	})
	assert.Equal(t, "/s", r.searchPath)
	assert.Equal(t, "/k", r.skillsPath)
	assert.Equal(t, "/d", r.downloadPath)
	assert.Equal(t, 3*time.Second, r.client.Timeout)
	assert.Equal(t, 10, r.maxZipSize)
	assert.Equal(t, 20, r.maxResponseSize)

	d := NewClawHubRegistry(ClawHubConfig{})
	assert.Equal(t, "https://clawhub.ai", d.baseURL)
}

func TestClawHubRegistry_BadResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/skills/missing") {
			http.Error(w, "no such skill", http.StatusNotFound)
			return
		}
		writeBody(t, w, []byte("not json"))
	}))
	defer srv.Close()
	reg := newTestRegistry(srv.URL, "")
	ctx := context.Background()

	_, err := reg.Search(ctx, "x", 0)
	require.ErrorContains(t, err, "failed to parse search response")

	_, err = reg.GetSkillMeta(ctx, "alpha")
	require.ErrorContains(t, err, "failed to parse skill metadata")

	_, err = reg.GetSkillMeta(ctx, "missing")
	require.ErrorContains(t, err, "HTTP 404: no such skill")
}

func TestClawHubRegistry_InvalidBaseURL(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry("://bad", "")
	_, err := reg.Search(ctx, "x", 1)
	require.ErrorContains(t, err, "invalid base URL")

	// A base URL that parses but cannot form a request.
	bad := newTestRegistry("http://a b", "")
	_, err = bad.GetSkillMeta(ctx, "alpha")
	require.ErrorContains(t, err, "skill metadata request failed")

	t.Setenv("TMPDIR", t.TempDir())
	_, err = reg.DownloadAndInstall(ctx, "alpha", "1.0", t.TempDir())
	require.ErrorContains(t, err, "invalid base URL")
	_, err = bad.DownloadAndInstall(ctx, "alpha", "1.0", t.TempDir())
	require.ErrorContains(t, err, "invalid base URL")
}

func TestClawHubRegistry_DownloadAndInstallOutcomes(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	zipData := createTestZip(t, map[string]string{"SKILL.md": "# alpha"})

	var mu sync.Mutex
	var versions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/skills/"):
			// No metadata: install proceeds without it.
			http.NotFound(w, r)
		case slug == "gone":
			http.Error(w, "gone", http.StatusGone)
		case slug == "big":
			writeBody(t, w, make([]byte, 64))
		case slug == "notzip":
			writeBody(t, w, []byte("garbage"))
		default:
			mu.Lock()
			versions = append(versions, r.URL.Query().Get("version"))
			mu.Unlock()
			writeBody(t, w, zipData)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	reg := newTestRegistry(srv.URL, "")

	target := filepath.Join(t.TempDir(), "alpha")
	res, err := reg.DownloadAndInstall(ctx, "alpha", "", target)
	require.NoError(t, err)
	assert.Equal(t, "latest", res.Version, "no metadata and no version means latest")
	data, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# alpha", string(data))

	res, err = reg.DownloadAndInstall(ctx, "alpha", "2.0", filepath.Join(t.TempDir(), "a2"))
	require.NoError(t, err)
	assert.Equal(t, "2.0", res.Version)
	assert.Equal(t, []string{"", "2.0"}, versions, "latest sends no version parameter")

	_, err = reg.DownloadAndInstall(ctx, "../evil", "", t.TempDir())
	require.ErrorContains(t, err, "invalid slug")

	_, err = reg.DownloadAndInstall(ctx, "gone", "", t.TempDir())
	require.ErrorContains(t, err, "HTTP 410: gone")

	_, err = reg.DownloadAndInstall(ctx, "notzip", "", t.TempDir())
	require.ErrorContains(t, err, "invalid ZIP")

	small := NewClawHubRegistry(ClawHubConfig{BaseURL: srv.URL, MaxZipSize: 10})
	_, err = small.DownloadAndInstall(ctx, "big", "", t.TempDir())
	require.ErrorContains(t, err, "download too large")

	// Every temp download was removed.
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	require.NoError(t, err)
	assert.Empty(t, entries)

	// No writable temp dir.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, err = reg.DownloadAndInstall(ctx, "alpha", "", t.TempDir())
	require.ErrorContains(t, err, "failed to create temp file")
}

func TestClawHubRegistry_DownloadTruncated(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/skills/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "1000")
		writeBody(t, w, []byte("partial"))
	}))
	defer srv.Close()
	_, err := newTestRegistry(srv.URL, "").DownloadAndInstall(context.Background(), "alpha", "", t.TempDir())
	require.ErrorContains(t, err, "download write failed")
}

// --- GitHub installer ---

// githubFake answers the installer's requests to api.github.com and
// raw.githubusercontent.com from one httptest server, so no request leaves
// the machine.
type githubFake struct {
	mu   sync.Mutex
	auth map[string]string // host+path -> Authorization header
}

type rewriteTransport struct {
	target *url.URL
	next   http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Orig-Host", r.URL.Host)
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	return rt.next.RoundTrip(r)
}

// newFakeGitHub starts srv and points installer's client at it.
func newFakeGitHub(t *testing.T, installer *SkillInstaller, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	installer.client = &http.Client{Transport: rewriteTransport{target: target, next: http.DefaultTransport}}
}

func TestInstallFromGitHub_ViaAPI(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	installer, err := NewSkillInstaller(ws, "gh-token", "")
	require.NoError(t, err)

	fake := &githubFake{auth: map[string]string{}}
	newFakeGitHub(t, installer, func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Orig-Host")
		fake.mu.Lock()
		fake.auth[host+r.URL.Path] = r.Header.Get("Authorization")
		fake.mu.Unlock()
		switch host + r.URL.Path {
		case "api.github.com/repos/alice/tools/contents/skills/weather":
			assert.Equal(t, "dev", r.URL.Query().Get("ref"))
			encodeBody(t, w, []GitHubContent{
				{Name: "SKILL.md", Type: "file", DownloadURL: "https://raw.githubusercontent.com/f/SKILL.md"},
				{Name: "README.md", Type: "file", DownloadURL: "https://raw.githubusercontent.com/f/README.md"},
				{Name: "scripts", Type: "dir", URL: "https://api.github.com/sub/scripts"},
				{Name: "node_modules", Type: "dir", URL: "https://api.github.com/sub/node_modules"},
			})
		case "api.github.com/sub/scripts":
			encodeBody(t, w, []GitHubContent{
				{Name: "run.sh", Type: "file", DownloadURL: "https://raw.githubusercontent.com/f/run.sh"},
			})
		case "raw.githubusercontent.com/f/SKILL.md":
			writeBody(t, w, []byte("# weather"))
		case "raw.githubusercontent.com/f/run.sh":
			writeBody(t, w, []byte("echo hi"))
		default:
			t.Errorf("unexpected request %s%s", host, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	err = installer.InstallFromGitHub(context.Background(), "https://github.com/alice/tools/tree/dev/skills/weather")
	require.NoError(t, err)

	dir := filepath.Join(ws, "skills", "weather")
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# weather", string(data))
	data, err = os.ReadFile(filepath.Join(dir, "scripts", "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, "echo hi", string(data))
	_, err = os.Stat(filepath.Join(dir, "README.md"))
	assert.True(t, os.IsNotExist(err), "only SKILL.md is taken from the skill root")
	_, err = os.Stat(filepath.Join(dir, "node_modules"))
	assert.True(t, os.IsNotExist(err), "non-resource directories are skipped")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, "Bearer gh-token", fake.auth["api.github.com/repos/alice/tools/contents/skills/weather"])
}

func TestInstallFromGitHub_FallsBackToRaw(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	installer, err := NewSkillInstaller(ws, "", "")
	require.NoError(t, err)
	newFakeGitHub(t, installer, func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Orig-Host")
		switch {
		case host == "api.github.com":
			http.Error(w, "rate limited", http.StatusForbidden)
		case host == "raw.githubusercontent.com" && r.URL.Path == "/alice/tools/main/SKILL.md":
			writeBody(t, w, []byte("# tools"))
		default:
			http.NotFound(w, r)
		}
	})

	require.NoError(t, installer.InstallFromGitHub(context.Background(), "alice/tools"))
	data, err := os.ReadFile(filepath.Join(ws, "skills", "tools", "SKILL.md"))
	require.NoError(t, err)
	assert.Equal(t, "# tools", string(data))

	// Neither the API nor raw has it.
	err = installer.InstallFromGitHub(context.Background(), "alice/missing")
	require.ErrorContains(t, err, "failed to fetch skill")
}

func TestInstallFromGitHub_NoSkillFile(t *testing.T) {
	ws := t.TempDir()
	installer, err := NewSkillInstaller(ws, "", "")
	require.NoError(t, err)
	newFakeGitHub(t, installer, func(w http.ResponseWriter, _ *http.Request) {
		encodeBody(t, w, []GitHubContent{{Name: "README.md", Type: "file"}})
	})
	err = installer.InstallFromGitHub(context.Background(), "alice/tools")
	require.EqualError(t, err, "SKILL.md not found in repository")

	err = installer.InstallFromGitHub(context.Background(), "nope")
	require.ErrorContains(t, err, "expected 'owner/repo'")
}

func TestGetGithubDirAllFiles_Errors(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	installer, err := NewSkillInstaller(ws, "", "")
	require.NoError(t, err)
	newFakeGitHub(t, installer, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/badjson":
			writeBody(t, w, []byte("{"))
		case "/baddownload":
			encodeBody(t, w, []GitHubContent{{Name: "SKILL.md", Type: "file", DownloadURL: "https://raw.githubusercontent.com/missing"}})
		case "/badsub":
			encodeBody(t, w, []GitHubContent{{Name: "docs", Type: "dir", URL: "https://api.github.com/badjson"}})
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	dir := filepath.Join(ws, "out")
	assert.Error(t, installer.getGithubDirAllFiles(ctx, "https://api.github.com/badjson", dir, true))
	assert.ErrorContains(t, installer.getGithubDirAllFiles(ctx, "https://api.github.com/baddownload", dir, true), "download SKILL.md")
	assert.Error(t, installer.getGithubDirAllFiles(ctx, "https://api.github.com/badsub", dir, true))
	assert.Error(t, installer.getGithubDirAllFiles(ctx, "http://a b/", dir, true), "an unparseable URL")
}

func TestDownloadFileAndRaw_LocalErrors(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	installer, err := NewSkillInstaller(ws, "", "")
	require.NoError(t, err)
	newFakeGitHub(t, installer, func(w http.ResponseWriter, _ *http.Request) {
		writeBody(t, w, []byte("content"))
	})
	ctx := context.Background()

	blocker := filepath.Join(ws, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	assert.Error(t, installer.downloadFile(ctx, "https://raw.githubusercontent.com/x", filepath.Join(blocker, "sub", "f")))
	assert.ErrorContains(t, installer.downloadRaw(ctx, "alice", "tools", "main", "", filepath.Join(blocker, "sub")), "failed to create skill directory")

	// The destination is a non-empty directory: the move fails.
	dest := filepath.Join(ws, "dest")
	require.NoError(t, os.MkdirAll(filepath.Join(dest, "SKILL.md", "x"), 0o700))
	assert.ErrorContains(t, installer.downloadFile(ctx, "https://raw.githubusercontent.com/x", filepath.Join(dest, "SKILL.md")), "failed to move downloaded file")
	assert.ErrorContains(t, installer.downloadRaw(ctx, "alice", "tools", "main", "sub", dest), "failed to write skill file")

	assert.Error(t, installer.downloadFile(ctx, "http://a b/", filepath.Join(ws, "f")))

	// Nothing is left in the temp dir.
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}
