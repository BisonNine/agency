package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
)

// fakeDNS answers lookups, pre-flight and at dial time, from answers (or
// with err) for the test.
func fakeDNS(t *testing.T, answers map[string][]string, err error) {
	t.Helper()
	previous, previousDial := lookupIPAddr, dialLookupIPAddr
	t.Cleanup(func() { lookupIPAddr, dialLookupIPAddr = previous, previousDial })
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if err != nil {
			return nil, err
		}
		var out []net.IPAddr
		for _, a := range answers[host] {
			out = append(out, net.IPAddr{IP: net.ParseIP(a)})
		}
		return out, nil
	}
	dialLookupIPAddr = lookupIPAddr
}

// --- the dial-time guard ---

func TestSafeDialContext_Refusals(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	dial := newSafeDialContext(&net.Dialer{Timeout: time.Second})
	ctx := context.Background()

	cases := []struct {
		address string
		dns     map[string][]string
		dnsErr  error
		want    string
	}{
		{"no-port", nil, nil, `invalid target address "no-port"`},
		{":80", nil, nil, "empty target host"},
		{"127.0.0.1:80", nil, nil, "blocked private or local target: 127.0.0.1"},
		{"169.254.169.254:80", nil, nil, "blocked private or local target: 169.254.169.254"},
		{"[::1]:80", nil, nil, "blocked private or local target: ::1"},
		{"rebind.example:80", map[string][]string{"rebind.example": {"10.0.0.5", "192.168.1.1"}}, nil, "all resolved addresses for rebind.example are private or restricted"},
		{"nowhere.example:80", nil, errors.New("no such host"), "failed to resolve nowhere.example: no such host"},
		{"empty.example:80", map[string][]string{}, nil, "all resolved addresses for empty.example are private or restricted"},
	}
	for _, tc := range cases {
		fakeDNS(t, tc.dns, tc.dnsErr)
		conn, err := dial(ctx, "tcp", tc.address)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.address, tc.want, err)
		}
	}
}

// A name resolving to a mix of private and public addresses is dialled on
// the public ones only; when those fail, the error says so.
func TestSafeDialContext_PublicAddressesOnly(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	fakeDNS(t, map[string][]string{"mixed.example": {"10.0.0.1", "203.0.113.7"}}, nil)
	dial := newSafeDialContext(&net.Dialer{Timeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no connection is made
	_, err := dial(ctx, "tcp", "mixed.example:80")
	if err == nil || !strings.Contains(err.Error(), "failed connecting to public addresses for mixed.example") ||
		!strings.Contains(err.Error(), "203.0.113.7") {
		t.Errorf("dial: %v", err)
	}
}

// The redirect policy: a limit, no private targets, and a proxied target is
// checked by name.
func TestWebFetch_RedirectPolicy(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	req := func(raw string) *http.Request {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	direct, err := NewWebFetchToolWithProxy(0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	check := direct.client.CheckRedirect

	if err := check(req("https://example.com/"), make([]*http.Request, maxRedirects)); err == nil || err.Error() != "stopped after 5 redirects" {
		t.Errorf("redirect limit: %v", err)
	}
	for _, target := range []string{"http://localhost/x", "http://10.1.2.3/", "http://[fd00::1]/", "http://169.254.169.254/latest/meta-data"} {
		if err := check(req(target), nil); err == nil || err.Error() != "redirect target is private or local network host" {
			t.Errorf("redirect to %s: %v", target, err)
		}
	}
	if err := check(req("https://example.com/next"), nil); err != nil {
		t.Errorf("public redirect: %v", err)
	}

	proxied, err := NewWebFetchToolWithProxy(0, "http://proxy.example:3128", 0)
	if err != nil {
		t.Fatal(err)
	}
	fakeDNS(t, map[string][]string{"inside.example": {"192.168.0.10"}, "outside.example": {"203.0.113.9"}}, nil)
	if err := proxied.client.CheckRedirect(req("https://inside.example/"), nil); err == nil || !strings.Contains(err.Error(), "inside.example resolves to 192.168.0.10") {
		t.Errorf("proxied private redirect: %v", err)
	}
	if err := proxied.client.CheckRedirect(req("https://outside.example/"), nil); err != nil {
		t.Errorf("proxied public redirect: %v", err)
	}
}

func TestCheckProxiedTarget_AllowedAndUnresolved(t *testing.T) {
	fakeDNS(t, map[string][]string{"inside.example": {"10.0.0.1"}}, nil)
	allowPrivateWebFetchHosts.Store(true)
	if err := checkProxiedTarget(context.Background(), "inside.example"); err != nil {
		t.Errorf("allowed for tests: %v", err)
	}
	allowPrivateWebFetchHosts.Store(false)
	fakeDNS(t, nil, errors.New("no local DNS"))
	if err := checkProxiedTarget(context.Background(), "unresolved.example"); err != nil {
		t.Errorf("unresolved: %v", err)
	}
}

func TestIsPrivateOrRestrictedIP_Nil(t *testing.T) {
	if !isPrivateOrRestrictedIP(nil) {
		t.Error("a nil address is refused")
	}
}

func TestNewWebFetchToolWithProxy_BadProxy(t *testing.T) {
	if _, err := NewWebFetchToolWithProxy(0, "ftp://proxy.example", 0); err == nil ||
		!strings.Contains(err.Error(), "failed to create HTTP client for web fetch") {
		t.Errorf("bad proxy: %v", err)
	}
}

// --- web_fetch ---

func TestWebFetch_ResponsesAndArguments(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/badjson":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{not json"))
		case "/js":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><script>" + strings.Repeat("var a=1;", 300) + "</script><body>hi</body></html>"))
		case "/long":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("abcdefghij", 50)))
		case "/cut":
			// Promise more than is sent, then hang up.
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("partial"))
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
		}
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(150, testFetchLimit)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	decode := func(forLLM string) map[string]any {
		t.Helper()
		start, end := strings.Index(forLLM, "{"), strings.LastIndex(forLLM, "}")
		var m map[string]any
		if start < 0 || end < start || json.Unmarshal([]byte(forLLM[start:end+1]), &m) != nil {
			t.Fatalf("no JSON in %q", forLLM)
		}
		return m
	}

	res := tool.Execute(ctx, map[string]any{"url": server.URL + "/badjson"})
	if m := decode(res.ForLLM); res.IsError || m["extractor"] != "raw" || m["text"] != "{not json" {
		t.Errorf("invalid JSON body: %v", m)
	}
	res = tool.Execute(ctx, map[string]any{"url": server.URL + "/js"})
	if m := decode(res.ForLLM); res.IsError || m["extractor"] != "text" || !strings.Contains(m["note"].(string), "JavaScript-rendered") {
		t.Errorf("script page: %v", m)
	}
	// maxChars above 100 overrides the tool's limit; at or below, it is ignored.
	res = tool.Execute(ctx, map[string]any{"url": server.URL + "/long", "maxChars": 300.0})
	if m := decode(res.ForLLM); m["length"] != 300.0 || m["truncated"] != true {
		t.Errorf("maxChars 300: %v", m)
	}
	res = tool.Execute(ctx, map[string]any{"url": server.URL + "/long", "maxChars": 50.0})
	if m := decode(res.ForLLM); m["length"] != 150.0 {
		t.Errorf("maxChars 50: %v", m)
	}
	res = tool.Execute(ctx, map[string]any{"url": server.URL + "/cut"})
	if !res.IsError || !strings.Contains(res.ForLLM, "failed to read response: unexpected EOF") {
		t.Errorf("cut body: %s", res.ForLLM)
	}
	res = tool.Execute(ctx, map[string]any{"url": "http://[::1"})
	if !res.IsError || !strings.Contains(res.ForLLM, "invalid URL") {
		t.Errorf("unparsable URL: %s", res.ForLLM)
	}
}

// --- the global provider ---

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "web" || GlobalProvider.Description() != "Web search and fetch operations" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}

func handlers(t *testing.T, deps toolspec.Deps) map[string]toolspec.ToolHandler {
	t.Helper()
	out := map[string]toolspec.ToolHandler{}
	for _, d := range GlobalProvider.RegisterTools(deps) {
		out[d.Name] = d.Handler
	}
	return out
}

// Without a config the tools are advertised but say they are not configured.
func TestGlobalProvider_NotConfigured(t *testing.T) {
	h := handlers(t, toolspec.Deps{})
	for name, want := range map[string]string{
		"search": "web search is not configured on this server (no search provider is enabled)",
		"fetch":  "web fetch is not configured on this server",
	} {
		res, err := h[name](&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"query": "x", "url": "https://example.com"}})
		if err != nil || !res.IsError || res.ForLLM != want {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
}

// From a config: the configured search provider answers, and fetch keeps
// its private-host refusal.
func TestGlobalProvider_FromConfig(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"results":[{"title":"Alice's page","url":"https://alice.example","content":"hello"}]}`))
	}))
	defer server.Close()

	c := &config.Config{}
	c.Tools.Web.SearXNG.Enabled = true
	c.Tools.Web.SearXNG.BaseURL = server.URL
	h := handlers(t, toolspec.Deps{Cfg: c})

	res, err := h["search"](&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"query": "alice"}})
	if err != nil || res.IsError || !strings.Contains(res.ForLLM, "Alice's page") || hits.Load() != 1 {
		t.Errorf("search: %v %+v", err, res)
	}
	res, err = h["fetch"](&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{"url": "http://127.0.0.1/admin"}})
	if err != nil || !res.IsError || res.ForLLM != "fetching private or local network hosts is not allowed" {
		t.Errorf("fetch private: %v %+v", err, res)
	}
}

// --- search providers ---

// scripted serves the given status/body pairs in order, recording the
// credentials each request carried.
type scripted struct {
	steps []struct {
		status int
		body   string
	}
	n    atomic.Int32
	auth []string
}

func (s *scripted) add(status int, body string) *scripted {
	s.steps = append(s.steps, struct {
		status int
		body   string
	}{status, body})
	return s
}

func (s *scripted) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(s.n.Add(1)) - 1
		s.auth = append(s.auth, r.Header.Get("Authorization")+r.Header.Get("X-Subscription-Token"))
		if i >= len(s.steps) {
			i = len(s.steps) - 1
		}
		w.WriteHeader(s.steps[i].status)
		_, _ = w.Write([]byte(s.steps[i].body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingTransport fails every request.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

func TestPerplexitySearchProvider(t *testing.T) {
	ctx := context.Background()
	newP := func(srv *httptest.Server, keys ...string) *PerplexitySearchProvider {
		return &PerplexitySearchProvider{keyPool: NewAPIKeyPool(keys), client: &http.Client{Transport: &rewriteTransport{base: srv.URL}}}
	}

	s := (&scripted{}).add(http.StatusTooManyRequests, "slow down").add(http.StatusOK, `{"choices":[{"message":{"content":"1. Go\n   https://go.dev"}}]}`)
	got, err := newP(s.server(t), "k1", "k2").Search(ctx, "golang", 3)
	if err != nil || got != "Results for: golang (via Perplexity)\n1. Go\n   https://go.dev" {
		t.Errorf("failover: %q %v", got, err)
	}
	if len(s.auth) != 2 || s.auth[0] == s.auth[1] {
		t.Errorf("each key tried once: %v", s.auth)
	}

	s = (&scripted{}).add(http.StatusOK, `{"choices":[]}`)
	if got, err := newP(s.server(t), "k").Search(ctx, "q", 3); err != nil || got != "No results for: q" {
		t.Errorf("no results: %q %v", got, err)
	}
	s = (&scripted{}).add(http.StatusOK, `not json`)
	if _, err := newP(s.server(t), "k").Search(ctx, "q", 3); err == nil || !strings.Contains(err.Error(), "failed to parse response") {
		t.Errorf("bad JSON: %v", err)
	}
	s = (&scripted{}).add(http.StatusBadRequest, `bad query`)
	if _, err := newP(s.server(t), "k1", "k2").Search(ctx, "q", 3); err == nil || err.Error() != "perplexity API error: bad query" || s.n.Load() != 1 {
		t.Errorf("non-retryable: %v after %d calls", err, s.n.Load())
	}
	s = (&scripted{}).add(http.StatusInternalServerError, `down`)
	if _, err := newP(s.server(t), "k1", "k2").Search(ctx, "q", 3); err == nil || !strings.Contains(err.Error(), "all api keys failed, last error: perplexity API error: down") {
		t.Errorf("all failed: %v", err)
	}
	p := &PerplexitySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), client: &http.Client{Transport: failingTransport{}}}
	if _, err := p.Search(ctx, "q", 3); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("transport failure: %v", err)
	}
}

func TestBraveSearchProvider_FailoverAndLimits(t *testing.T) {
	ctx := context.Background()
	newP := func(srv *httptest.Server, keys ...string) *BraveSearchProvider {
		return &BraveSearchProvider{keyPool: NewAPIKeyPool(keys), client: &http.Client{Transport: &rewriteTransport{base: srv.URL}}}
	}
	s := (&scripted{}).add(http.StatusUnauthorized, "bad key").
		add(http.StatusOK, `{"web":{"results":[{"title":"A","url":"https://a.example"},{"title":"B","url":"https://b.example"}]}}`)
	got, err := newP(s.server(t), "k1", "k2").Search(ctx, "q", 1)
	if err != nil || got != "Results for: q\n1. A\n   https://a.example" {
		t.Errorf("failover and count: %q %v", got, err)
	}
	s = (&scripted{}).add(http.StatusOK, `{`)
	if _, err := newP(s.server(t), "k").Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to parse response") {
		t.Errorf("bad JSON: %v", err)
	}
	p := &BraveSearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), client: &http.Client{Transport: failingTransport{}}}
	if _, err := p.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "all api keys failed, last error: request failed") {
		t.Errorf("transport failure: %v", err)
	}
}

func TestTavilySearchProvider_FailoverAndLimits(t *testing.T) {
	ctx := context.Background()
	s := (&scripted{}).add(http.StatusOK, `{"results":[{"title":"A","url":"https://a.example","content":"aa"},{"title":"B","url":"https://b.example"}]}`)
	srv := s.server(t)
	p := &TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), baseURL: srv.URL, client: srv.Client()}
	got, err := p.Search(ctx, "q", 1)
	if err != nil || got != "Results for: q (via Tavily)\n1. A\n   https://a.example\n   aa" {
		t.Errorf("count: %q %v", got, err)
	}
	s = (&scripted{}).add(http.StatusOK, `[`)
	srv = s.server(t)
	p = &TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), baseURL: srv.URL, client: srv.Client()}
	if _, err := p.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to parse response") {
		t.Errorf("bad JSON: %v", err)
	}
	p = &TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), baseURL: "http://tavily.example", client: &http.Client{Transport: failingTransport{}}}
	if _, err := p.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "all api keys failed, last error: request failed") {
		t.Errorf("transport failure: %v", err)
	}
	p = &TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), baseURL: "http://[::1", client: http.DefaultClient}
	if _, err := p.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("bad base URL: %v", err)
	}
}

func TestGLMAndSearXNGProviders_Failures(t *testing.T) {
	ctx := context.Background()
	glm := &GLMSearchProvider{apiKey: "k", baseURL: "http://glm.example", client: &http.Client{Transport: failingTransport{}}}
	if _, err := glm.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("GLM transport failure: %v", err)
	}
	glm.baseURL = "http://[::1"
	if _, err := glm.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("GLM bad URL: %v", err)
	}
	s := (&scripted{}).add(http.StatusOK, `{"search_result":[{"title":"A","link":"https://a.example"},{"title":"B","link":"https://b.example"}]}`)
	srv := s.server(t)
	glm = &GLMSearchProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	if got, err := glm.Search(ctx, "q", 1); err != nil || got != "Results for: q (via GLM Search)\n1. A\n   https://a.example" {
		t.Errorf("GLM count: %q %v", got, err)
	}

	sx := &SearXNGSearchProvider{baseURL: "http://searx.example", client: &http.Client{Transport: failingTransport{}}}
	if _, err := sx.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("SearXNG transport failure: %v", err)
	}
	sx.baseURL = "http://[::1"
	if _, err := sx.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("SearXNG bad URL: %v", err)
	}
}

func TestDuckDuckGo_RedirectLinksAndFailures(t *testing.T) {
	p := &DuckDuckGoSearchProvider{}
	page := `<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Falice.example%2Fpage&amp;rut=x">Alice <b>page</b></a>` +
		`<a class="result__snippet" href="x">About <b>Alice</b></a>`
	got, err := p.extractResults(page, 5, "alice")
	if err != nil || !strings.Contains(got, "1. Alice page\n   https://alice.example/page") || !strings.Contains(got, "   About Alice") {
		t.Errorf("extract: %q %v", got, err)
	}
	p.client = &http.Client{Transport: failingTransport{}}
	if _, err := p.Search(context.Background(), "q", 1); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("transport failure: %v", err)
	}
}

// --- choosing a provider ---

func TestNewWebSearchTool_PerplexityFirstAndBadProxy(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{
		PerplexityEnabled: true, PerplexityAPIKeys: []string{"p"}, PerplexityMaxResults: 7,
		BraveEnabled: true, BraveAPIKeys: []string{"b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tool.provider.(*PerplexitySearchProvider); !ok || tool.maxResults != 7 {
		t.Errorf("provider %T, max %d", tool.provider, tool.maxResults)
	}

	for name, opts := range map[string]WebSearchToolOptions{
		"Perplexity": {PerplexityEnabled: true, PerplexityAPIKeys: []string{"p"}},
		"Brave":      {BraveEnabled: true, BraveAPIKeys: []string{"b"}},
		"SearXNG":    {SearXNGEnabled: true, SearXNGBaseURL: "https://searx.example"},
		"Tavily":     {TavilyEnabled: true, TavilyAPIKeys: []string{"t"}},
		"DuckDuckGo": {DuckDuckGoEnabled: true},
		"GLM Search": {GLMSearchEnabled: true, GLMSearchAPIKey: "g"},
	} {
		opts.Proxy = "ftp://proxy.example"
		if _, err := NewWebSearchTool(opts); err == nil || !strings.Contains(err.Error(), "failed to create HTTP client for "+name) {
			t.Errorf("%s with a bad proxy: %v", name, err)
		}
	}
}

func TestIsLoopbackURL_Unparsable(t *testing.T) {
	if isLoopbackURL("http://[::1") {
		t.Error("an unparsable URL is not loopback")
	}
	if !isLoopbackURL("http://LOCALHOST:8888") || !isLoopbackURL("http://127.0.0.2/") || isLoopbackURL((&url.URL{Scheme: "http", Host: "searx.example"}).String()) {
		t.Error("isLoopbackURL")
	}
}

// recordingTransport records the URLs asked for and fails every request.
type recordingTransport struct{ urls []string }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return nil, errors.New("network down")
}

// Without a base URL, Tavily and GLM Search use their public endpoints.
func TestSearchProviders_DefaultEndpoints(t *testing.T) {
	ctx := context.Background()
	rec := &recordingTransport{}
	_, _ = (&TavilySearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), client: &http.Client{Transport: rec}}).Search(ctx, "q", 1)
	_, _ = (&GLMSearchProvider{apiKey: "k", client: &http.Client{Transport: rec}}).Search(ctx, "q", 1)
	want := []string{"https://api.tavily.com/search", "https://open.bigmodel.cn/api/paas/v4/web_search"}
	if len(rec.urls) != 2 || rec.urls[0] != want[0] || rec.urls[1] != want[1] {
		t.Errorf("endpoints = %v", rec.urls)
	}
}

// A response cut short is a failure for that key; the next key is tried.
func TestSearchProviders_CutResponse(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("{"))
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	rewrite := &http.Client{Transport: &rewriteTransport{base: srv.URL}}
	keys := NewAPIKeyPool([]string{"k1", "k2"})

	for name, search := range map[string]func() (string, error){
		"brave": func() (string, error) {
			return (&BraveSearchProvider{keyPool: keys, client: rewrite}).Search(ctx, "q", 1)
		},
		"tavily": func() (string, error) {
			return (&TavilySearchProvider{keyPool: keys, baseURL: srv.URL, client: srv.Client()}).Search(ctx, "q", 1)
		},
		"perplexity": func() (string, error) {
			return (&PerplexitySearchProvider{keyPool: keys, client: rewrite}).Search(ctx, "q", 1)
		},
	} {
		calls.Store(0)
		if _, err := search(); err == nil || !strings.Contains(err.Error(), "all api keys failed, last error: failed to read response") || calls.Load() != 2 {
			t.Errorf("%s: %v after %d calls", name, err, calls.Load())
		}
	}
	if _, err := (&DuckDuckGoSearchProvider{client: rewrite}).Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to read response") {
		t.Errorf("duckduckgo: %v", err)
	}
	if _, err := (&GLMSearchProvider{apiKey: "k", baseURL: srv.URL, client: srv.Client()}).Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "failed to read response") {
		t.Errorf("glm: %v", err)
	}
}
