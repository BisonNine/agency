package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/cmd/agency-auth/config"
	"go.bison9.com/agency/cmd/agency-auth/mcp"
	"go.bison9.com/agency/cmd/agency-auth/providers"
)

// stubProvider is an OAuthProvider whose endpoints and behaviour a test sets.
type stubProvider struct {
	name                string
	authURL, tokenURL   string
	device, authCode    bool
	validateErr         error
	customizeErr        error
	processErr          error
	customizeTokenExtra map[string]string
	userErr             error
}

func (s *stubProvider) GetServiceName() string           { return s.name }
func (s *stubProvider) GetDisplayName() string           { return "Stub" }
func (s *stubProvider) GetRequiredScopes() []string      { return []string{"scope-a"} }
func (s *stubProvider) GetAuthorizationEndpoint() string { return s.authURL }
func (s *stubProvider) GetTokenEndpoint() string         { return s.tokenURL }
func (s *stubProvider) GetDeviceCodeEndpoint() string    { return "" }
func (s *stubProvider) SupportsDeviceFlow() bool         { return s.device }
func (s *stubProvider) SupportsAuthorizationCode() bool  { return s.authCode }
func (s *stubProvider) ValidateConfiguration(*providers.ServiceConfig) error {
	return s.validateErr
}

func (s *stubProvider) CustomizeDeviceRequest(map[string]string, *providers.ServiceConfig) error {
	return nil
}

func (s *stubProvider) CustomizeTokenRequest(params map[string]string, _ *providers.ServiceConfig) error {
	maps.Copy(params, s.customizeTokenExtra)
	return s.customizeErr
}

func (s *stubProvider) ProcessTokenResponse(resp map[string]any) (*providers.TokenInfo, error) {
	if s.processErr != nil {
		return nil, s.processErr
	}
	at, ok := resp["access_token"].(string)
	if !ok {
		return nil, errors.New("no access_token")
	}
	return &providers.TokenInfo{AccessToken: at, ExpiresIn: 60}, nil
}

func (s *stubProvider) GetUserInfo(context.Context, *providers.TokenInfo) (*providers.UserInfo, error) {
	if s.userErr != nil {
		return nil, s.userErr
	}
	return &providers.UserInfo{Name: "Alice", Email: "alice@example.com"}, nil
}

func TestDecodeAuthBlob(t *testing.T) {
	raw := `{"u":"http://fusion:8888","c":"code-1","s":"google"}`
	for name, blob := range map[string]string{
		"unpadded": base64.RawURLEncoding.EncodeToString([]byte(raw)),
		"padded":   base64.URLEncoding.EncodeToString([]byte(raw + " ")),
	} {
		got, err := decodeAuthBlob(blob)
		if err != nil || got.URL != "http://fusion:8888" || got.Code != "code-1" || got.Service != "google" {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for blob, want := range map[string]string{
		"!!!": "failed to decode base64",
		base64.RawURLEncoding.EncodeToString([]byte("not json")):         "failed to parse JSON",
		base64.RawURLEncoding.EncodeToString([]byte(`{"u":"x","c":""}`)): "blob missing required fields (u, c, s)",
	} {
		if _, err := decodeAuthBlob(blob); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", blob, err, want)
		}
	}
}

func TestValidateFlags(t *testing.T) {
	for _, tc := range []struct {
		flags cliFlags
		want  string
	}{
		{cliFlags{}, "service is required (use -list to see available services)"},
		{cliFlags{service: "google"}, "fusion URL is required"},
		{cliFlags{service: "google", fusionURL: "http://f"}, "MCPFusion API token is required"},
		{cliFlags{service: "anything", fusionURL: "http://f", token: "t"}, ""},
	} {
		err := validateFlags(&tc.flags, nil)
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || err.Error() != tc.want)) {
			t.Errorf("%+v: %v, want %q", tc.flags, err, tc.want)
		}
	}
}

func TestCreateConfiguration(t *testing.T) {
	cfg, err := createConfiguration(&cliFlags{service: "google", fusionURL: "http://f", token: "t", verbose: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Service != "google" || cfg.FusionURL != "http://f" || cfg.APIToken != "t" || !cfg.Verbose ||
		cfg.Timeout != defaultTimeout || cfg.GetServiceConfig("google") == nil {
		t.Fatalf("cfg = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("created config is not valid: %v", err)
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	var out []byte
	var readErr error
	done := make(chan struct{})
	go func() {
		out, readErr = io.ReadAll(r)
		close(done)
	}()
	defer func() { os.Stdout = orig }()
	fn()
	closeOrFail(t, w)
	<-done
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out)
}

// closeOrFail closes c, failing the test on an error.
func closeOrFail(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

// writeBody writes body as a test server's response.
func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Error(err)
	}
}

// withStdin feeds input to the code under test as standard input.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	closeOrFail(t, w)
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		closeOrFail(t, r)
	})
}

func TestRegisterAndListProviders(t *testing.T) {
	reg := providers.NewProviderRegistry()
	if err := registerProviders(reg); err != nil {
		t.Fatal(err)
	}
	if err := registerProviders(reg); err == nil || !strings.Contains(err.Error(), "failed to register Google provider") {
		t.Fatalf("second registration: %v", err)
	}
	out := captureStdout(t, func() { listProviders(reg) })
	for _, want := range []string{"Available Providers:", "Service: google", "Display Name: Google APIs", "Auth Code Flow: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
}

// fakeFusion is an MCPFusion stand-in recording the token stores it gets.
type fakeFusion struct {
	mu      sync.Mutex
	ping    int
	config  string
	cfgCode int
	stored  []map[string]any
	storeOK bool
	url     string
}

func newFakeFusion(t *testing.T) *fakeFusion {
	t.Helper()
	f := &fakeFusion{ping: http.StatusOK, cfgCode: http.StatusOK, storeOK: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/ping":
			w.WriteHeader(f.ping)
			writeBody(t, w, `{"success":true,"tenant_id":"t1"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/services/"):
			w.WriteHeader(f.cfgCode)
			writeBody(t, w, f.config)
		case r.URL.Path == "/api/v1/oauth/tokens":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !f.storeOK {
				w.WriteHeader(http.StatusInternalServerError)
				writeBody(t, w, `{"message":"disk full"}`)
				return
			}
			f.stored = append(f.stored, body)
			writeBody(t, w, `{"success":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// set changes the fake's answers under its lock.
func (f *fakeFusion) set(ping, cfgCode int, cfgBody string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ping, f.cfgCode, f.config = ping, cfgCode, cfgBody
}

func (f *fakeFusion) storedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

func flowConfig(t *testing.T, f *fakeFusion, service string) *config.Config {
	t.Helper()
	cfg, err := createConfiguration(&cliFlags{service: service, fusionURL: f.url, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A service the server configures for user credentials prompts for each
// field and stores what was typed, with the sentinel access token.
func TestExecuteOAuthFlow_UserCredentials(t *testing.T) {
	f := newFakeFusion(t)
	f.set(http.StatusOK, http.StatusOK, `{"success":true,"config":{"auth_type":"user_credentials","instructions":"Get a key.",
		"fields":[{"name":"api_key","label":"API key","description":"From the portal"},{"name":"user"}]}}`)
	withStdin(t, "  k-123  \nalice\n")
	reg := providers.NewProviderRegistry()
	var err error
	out := captureStdout(t, func() {
		err = executeOAuthFlow(context.Background(), flowConfig(t, f, "portal"), &cliFlags{verbose: true}, reg)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Get a key.", "API key: From the portal", "Enter API key: ", "Enter user: ", "Credentials stored successfully for service 'portal'"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if f.storedCount() != 1 {
		t.Fatalf("stored = %v", f.stored)
	}
	s := f.stored[0]
	meta, ok := s["metadata"].(map[string]any)
	if !ok || s["service"] != "portal" || s["access_token"] != "user_credentials:portal" || meta["api_key"] != "k-123" || meta["user"] != "alice" {
		t.Fatalf("stored = %+v", s)
	}
}

func TestExecuteUserCredentialsFlow_Refusals(t *testing.T) {
	f := newFakeFusion(t)
	cfg := flowConfig(t, f, "portal")
	client := mcp.NewClient(f.url, "tok")
	data := &mcp.ServiceConfigData{Fields: []mcp.CredentialField{{Name: "api_key"}}}

	withStdin(t, "   \n")
	var err error
	captureStdout(t, func() { err = executeUserCredentialsFlow(context.Background(), cfg, client, data, false) })
	if err == nil || err.Error() != "value for 'api_key' cannot be empty" {
		t.Fatalf("empty: %v", err)
	}
	withStdin(t, "")
	captureStdout(t, func() { err = executeUserCredentialsFlow(context.Background(), cfg, client, data, false) })
	if err == nil || !strings.HasPrefix(err.Error(), "failed to read input for 'api_key'") {
		t.Fatalf("EOF: %v", err)
	}
	withStdin(t, "k\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeBody(t, w, `{"message":"refused"}`)
	}))
	defer srv.Close()
	bad := mcp.NewClient(srv.URL, "tok")
	captureStdout(t, func() { err = executeUserCredentialsFlow(context.Background(), cfg, bad, data, true) })
	if err == nil || !strings.Contains(err.Error(), "failed to store credentials in MCPFusion: token storage failed with status 400: refused") {
		t.Fatalf("store failure: %v", err)
	}
	if f.storedCount() != 0 {
		t.Fatal("refused input reached the store")
	}
}

// The flow stops early on each failure with a message naming it, and picks
// the flow the provider supports.
func TestExecuteOAuthFlow_Outcomes(t *testing.T) {
	f := newFakeFusion(t)

	f.set(http.StatusUnauthorized, http.StatusOK, "")
	err := executeOAuthFlow(context.Background(), flowConfig(t, f, "google"), &cliFlags{verbose: true}, providers.NewProviderRegistry())
	if err == nil || !strings.HasPrefix(err.Error(), "failed to connect to MCPFusion server: authentication failed: invalid API token") {
		t.Fatalf("ping failure: %v", err)
	}

	// A config fetch that fails falls back to local defaults; an unknown
	// service then has no provider.
	f.set(http.StatusOK, http.StatusNotFound, `{"message":"nope"}`)
	err = executeOAuthFlow(context.Background(), flowConfig(t, f, "nosuch"), &cliFlags{verbose: true}, providers.NewProviderRegistry())
	if err == nil || !strings.Contains(err.Error(), "no local OAuth provider registered for service 'nosuch'") {
		t.Fatalf("unknown service: %v", err)
	}

	f.set(http.StatusOK, http.StatusOK, `{"success":true,"config":{"auth_type":"oauth2","client_id":"cid","client_secret":"sec","scopes":"s"}}`)
	stub := &stubProvider{name: "stub", validateErr: errors.New("client_id missing")}
	reg := providers.NewProviderRegistry()
	if err = reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	cfg := flowConfig(t, f, "stub")
	err = executeOAuthFlow(context.Background(), cfg, &cliFlags{verbose: true}, reg)
	if err == nil || !strings.HasPrefix(err.Error(), "configuration validation failed: client_id missing") {
		t.Fatalf("validation: %v", err)
	}
	if got := cfg.GetServiceConfig("stub"); got == nil || got.ClientID != "cid" || got.ClientSecret != "sec" || got.Scope != "s" {
		t.Fatalf("server config not applied: %+v", got)
	}

	stub.validateErr = nil
	stub.device = true
	if err := executeOAuthFlow(context.Background(), flowConfig(t, f, "stub"), &cliFlags{verbose: true}, reg); err == nil || err.Error() != "device flow implementation pending" {
		t.Fatalf("device flow: %v", err)
	}
	stub.device = false
	if err := executeOAuthFlow(context.Background(), flowConfig(t, f, "stub"), &cliFlags{}, reg); err == nil || err.Error() != "provider 'stub' does not support any OAuth flow" {
		t.Fatalf("no flow: %v", err)
	}
}

func TestPKCEAndState(t *testing.T) {
	s1, err := generateSecureState()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := generateSecureState()
	if err != nil || s1 == s2 || len(s1) != 44 {
		t.Fatalf("states %q %q", s1, s2)
	}
	v, err := generateCodeVerifier()
	if err != nil || len(v) != 43 || strings.Contains(v, "=") {
		t.Fatalf("verifier %q %v", v, err)
	}
	sum := sha256.Sum256([]byte(v))
	if generateCodeChallenge(v) != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("challenge is not S256 of the verifier")
	}
}

func TestBuildAuthorizationURL(t *testing.T) {
	e := &OAuthFlowExecutor{Provider: &stubProvider{authURL: "https://login.example/${MS365_TENANT_ID}/authorize"}}
	raw, err := e.buildAuthorizationURL(&providers.ServiceConfig{ServiceName: "microsoft365", ClientID: "cid", TenantID: "ten", Scopes: "a b"},
		"http://127.0.0.1:5/callback", "st", "ch")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/ten/authorize" || q.Get("client_id") != "cid" || q.Get("response_type") != "code" || q.Get("scope") != "a b" ||
		q.Get("state") != "st" || q.Get("code_challenge") != "ch" || q.Get("code_challenge_method") != "S256" ||
		q.Get("prompt") != "consent" || q.Has("access_type") {
		t.Fatalf("url = %s", raw)
	}

	e.Provider = &stubProvider{authURL: "https://accounts.example/auth"}
	if raw, err = e.buildAuthorizationURL(&providers.ServiceConfig{ServiceName: "google"}, "r", "s", "c"); err != nil {
		t.Fatal(err)
	}
	if q := mustQuery(t, raw); q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Fatalf("google url = %s", raw)
	}
	e.Provider = &stubProvider{authURL: "http://%zz"}
	if _, err := e.buildAuthorizationURL(&providers.ServiceConfig{}, "r", "s", "c"); err == nil || !strings.HasPrefix(err.Error(), "invalid authorization endpoint") {
		t.Fatalf("bad endpoint: %v", err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// The callback accepts only /callback with the expected state and a code;
// everything else is an error page and an error result. Error text is
// escaped in the page.
func TestCallbackHandler(t *testing.T) {
	e := &OAuthFlowExecutor{}
	for _, tc := range []struct {
		path, wantErr string
		status        int
		code          string
	}{
		{"/elsewhere", "", http.StatusNotFound, ""},
		{"/callback?error=access_denied&error_description=<b>no</b>", "OAuth error: access_denied - <b>no</b>", http.StatusBadRequest, ""},
		{"/callback?error=access_denied", "OAuth error: access_denied", http.StatusBadRequest, ""},
		{"/callback?state=forged&code=c", "invalid state parameter - possible CSRF attack", http.StatusBadRequest, ""},
		{"/callback?state=good", "no authorization code received", http.StatusBadRequest, ""},
		{"/callback?state=good&code=c0de", "", http.StatusOK, "c0de"},
	} {
		results := make(chan authResult, 1)
		rec := httptest.NewRecorder()
		e.createCallbackHandler("good", "", nil, results).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.path, rec.Code, tc.status)
		}
		if tc.status == http.StatusNotFound {
			if len(results) != 0 {
				t.Errorf("%s: produced a result", tc.path)
			}
			continue
		}
		res := <-results
		switch {
		case tc.wantErr != "":
			if res.Error == nil || res.Error.Error() != tc.wantErr {
				t.Errorf("%s: result %+v, want error %q", tc.path, res, tc.wantErr)
			}
			if strings.Contains(rec.Body.String(), "<b>") {
				t.Errorf("%s: error page is not escaped", tc.path)
			}
		default:
			if res.Error != nil || res.Code != tc.code || !strings.Contains(rec.Body.String(), "OAuth Authentication Successful!") {
				t.Errorf("%s: result %+v body %q", tc.path, res, rec.Body.String())
			}
		}
	}
}

// The code exchange posts the PKCE form (with the provider's changes) and
// hands the response to the provider; every failure names its step.
func TestExchangeCodeForTokens(t *testing.T) {
	var form url.Values
	var path string
	status, body := http.StatusOK, `{"access_token":"at-1"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form, path = r.PostForm, r.URL.Path
		w.WriteHeader(status)
		writeBody(t, w, body)
	}))
	defer srv.Close()

	stub := &stubProvider{tokenURL: srv.URL + "/${MS365_TENANT_ID}/token", customizeTokenExtra: map[string]string{"extra": "1"}}
	e := &OAuthFlowExecutor{Provider: stub, Verbose: true}
	cfg := &providers.ServiceConfig{ClientID: "cid", ClientSecret: "sec", TenantID: "ten"}
	tok, err := e.exchangeCodeForTokens("c0de", "http://127.0.0.1:5/callback", "ver", cfg)
	if err != nil || tok.AccessToken != "at-1" {
		t.Fatalf("exchange = %+v %v", tok, err)
	}
	if path != "/ten/token" || form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "cid" ||
		form.Get("client_secret") != "sec" || form.Get("code") != "c0de" || form.Get("code_verifier") != "ver" ||
		form.Get("redirect_uri") != "http://127.0.0.1:5/callback" || form.Get("extra") != "1" {
		t.Fatalf("path %q form %v", path, form)
	}

	status, body = http.StatusBadRequest, "invalid_grant"
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || err.Error() != "token request failed with status 400: invalid_grant" {
		t.Fatalf("400: %v", err)
	}
	status, body = http.StatusOK, "x"
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || !strings.HasPrefix(err.Error(), "failed to parse token response") {
		t.Fatalf("bad JSON: %v", err)
	}
	body = `{}`
	stub.processErr = errors.New("no token")
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || err.Error() != "failed to process token response: no token" {
		t.Fatalf("process: %v", err)
	}
	stub.customizeErr = errors.New("nope")
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || err.Error() != "failed to customize token request: nope" {
		t.Fatalf("customize: %v", err)
	}
	stub.customizeErr = nil
	stub.tokenURL = "http://%zz"
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || !strings.HasPrefix(err.Error(), "failed to create token request") {
		t.Fatalf("bad URL: %v", err)
	}
	srv.Close()
	stub.tokenURL = srv.URL
	if _, err := e.exchangeCodeForTokens("c", "r", "v", cfg); err == nil || !strings.HasPrefix(err.Error(), "token request failed: ") {
		t.Fatalf("unreachable: %v", err)
	}
}

// authEnv is an executor for the stub provider whose token endpoint is a
// test server, storing into a fake MCPFusion.
type authEnv struct {
	exec   *OAuthFlowExecutor
	stub   *stubProvider
	fusion *fakeFusion

	mu        sync.Mutex
	tokenForm url.Values
}

// form is the last form the token endpoint received.
func (a *authEnv) form() url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenForm
}

// authCodeEnv builds an authEnv with the browser replaced by fn.
func authCodeEnv(t *testing.T, fn func(authURL string) error) *authEnv {
	t.Helper()
	env := &authEnv{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		env.mu.Lock()
		env.tokenForm = r.PostForm
		env.mu.Unlock()
		writeBody(t, w, `{"access_token":"at-1"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	f := newFakeFusion(t)
	stub := &stubProvider{name: "stub", authURL: "https://auth.example/authorize", tokenURL: tokenSrv.URL, authCode: true}
	cfg := flowConfig(t, f, "stub")
	cfg.SetServiceConfig("stub", &config.ServiceConfig{ClientID: "cid", Scope: "scope-a"})
	orig := launchBrowser
	launchBrowser = fn
	t.Cleanup(func() { launchBrowser = orig })
	env.exec = &OAuthFlowExecutor{Provider: stub, Config: cfg, MCPClient: mcp.NewClient(f.url, "tok"), Verbose: true}
	env.stub, env.fusion = stub, f
	return env
}

// browserVisiting returns a browser stand-in that follows the authorization
// URL back to its redirect URI with the given query, as the provider would.
func browserVisiting(t *testing.T, query func(state string) string, seen *url.Values) func(string) error {
	t.Helper()
	return func(authURL string) error {
		q := mustQuery(t, authURL)
		if seen != nil {
			*seen = q
		}
		resp, err := http.Get(q.Get("redirect_uri") + "?" + query(q.Get("state")))
		if err != nil {
			t.Errorf("callback: %v", err)
			return nil
		}
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		return nil
	}
}

// The authorization-code flow sends the PKCE challenge, takes the code back
// on the local callback, redeems it with the matching verifier and stores
// the tokens in MCPFusion.
func TestExecuteAuthCodeFlow_Success(t *testing.T) {
	var authQuery url.Values
	env := authCodeEnv(t, nil)
	e, f := env.exec, env.fusion
	launchBrowser = browserVisiting(t, func(state string) string { return "code=c0de&state=" + url.QueryEscape(state) }, &authQuery)
	var err error
	out := captureStdout(t, func() { err = e.ExecuteAuthCodeFlow(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "OAuth authentication successful!") || !strings.Contains(out, "Authenticated as: Alice (alice@example.com)") {
		t.Fatalf("output = %s", out)
	}
	if authQuery.Get("client_id") != "cid" || authQuery.Get("scope") != "scope-a" || authQuery.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization query = %v", authQuery)
	}
	form := env.form()
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if form.Get("code") != "c0de" || base64.RawURLEncoding.EncodeToString(sum[:]) != authQuery.Get("code_challenge") ||
		form.Get("redirect_uri") != authQuery.Get("redirect_uri") {
		t.Fatalf("token form %v does not match the authorization request %v", form, authQuery)
	}
	if f.storedCount() != 1 || f.stored[0]["access_token"] != "at-1" || f.stored[0]["service"] != "stub" {
		t.Fatalf("stored = %+v", f.stored)
	}
}

// Each later step's failure ends the flow with an error naming it, and
// nothing is stored.
func TestExecuteAuthCodeFlow_Failures(t *testing.T) {
	ok := func(state string) string { return "code=c0de&state=" + url.QueryEscape(state) }
	for _, tc := range []struct {
		name  string
		query func(string) string
		setup func(*stubProvider, *fakeFusion)
		want  string
	}{
		{"denied", func(string) string { return "error=access_denied" }, nil, "OAuth error: access_denied"},
		{"exchange", ok, func(s *stubProvider, _ *fakeFusion) { s.processErr = errors.New("bad token") }, "failed to exchange code for tokens: failed to process token response: bad token"},
		{"user info", ok, func(s *stubProvider, _ *fakeFusion) { s.userErr = errors.New("revoked") }, "failed to get user info: revoked"},
		{"store", ok, func(_ *stubProvider, f *fakeFusion) { f.mu.Lock(); f.storeOK = false; f.mu.Unlock() }, "failed to store tokens in MCPFusion: token storage failed with status 500: disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := authCodeEnv(t, nil)
			e, stub, f := env.exec, env.stub, env.fusion
			launchBrowser = browserVisiting(t, tc.query, nil)
			if tc.setup != nil {
				tc.setup(stub, f)
			}
			var err error
			captureStdout(t, func() { err = e.ExecuteAuthCodeFlow(context.Background()) })
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if f.storedCount() != 0 {
				t.Fatal("a failed flow stored tokens")
			}
		})
	}
}

// With no browser and the deadline passed, the flow gives up and says so.
func TestExecuteAuthCodeFlow_TimesOut(t *testing.T) {
	e := authCodeEnv(t, func(string) error { return errors.New("no display") }).exec
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var err error
	out := captureStdout(t, func() { err = e.ExecuteAuthCodeFlow(ctx) })
	if err == nil || err.Error() != "OAuth flow timed out" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "Please manually open the URL above in your browser.") {
		t.Fatalf("output = %s", out)
	}
}

// withArgs runs parseFlags on args with a fresh flag set.
func withArgs(t *testing.T, args ...string) cliFlags {
	t.Helper()
	origArgs, origSet := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = origArgs, origSet })
	os.Args = append([]string{"agency-auth"}, args...)
	flag.CommandLine = flag.NewFlagSet("agency-auth", flag.ContinueOnError)
	var f cliFlags
	parseFlags(&f)
	return f
}

// Flags fill the settings; a positional auth-code blob supplies service,
// server and token.
func TestParseFlags(t *testing.T) {
	f := withArgs(t, "-service", "google", "-fusion", "http://f", "-token", "t", "-verbose", "-debug", "-list", "-version")
	if f.service != "google" || f.fusionURL != "http://f" || f.token != "t" || !f.verbose || !f.debug || !f.list || !f.version {
		t.Fatalf("flags = %+v", f)
	}
	blob := base64.RawURLEncoding.EncodeToString([]byte(`{"u":"http://fusion:8888","c":"code-1","s":"google"}`))
	f = withArgs(t, blob)
	if f.service != "google" || f.fusionURL != "http://fusion:8888" || f.token != "code-1" {
		t.Fatalf("blob flags = %+v", f)
	}
	var usage strings.Builder
	flag.CommandLine.SetOutput(&usage)
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	flag.Usage()
	closeOrFail(t, w)
	os.Stderr = origStderr
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Usage: agency-auth <auth-code-blob>") {
		t.Fatalf("usage = %q", out)
	}
}

// A provider with only the authorization-code flow is run through it; an
// authorization endpoint that cannot be parsed stops it before any browser.
func TestExecuteOAuthFlow_AuthCode(t *testing.T) {
	f := newFakeFusion(t)
	f.set(http.StatusOK, http.StatusOK, `{"success":true,"config":{"auth_type":"oauth2","client_id":"cid"}}`)
	stub := &stubProvider{name: "stub", authURL: "https://auth.example/authorize", authCode: true}
	reg := providers.NewProviderRegistry()
	if err := reg.Register(stub); err != nil {
		t.Fatal(err)
	}
	orig := launchBrowser
	t.Cleanup(func() { launchBrowser = orig })
	launchBrowser = browserVisiting(t, func(string) string { return "error=access_denied" }, nil)
	var err error
	captureStdout(t, func() { err = executeOAuthFlow(context.Background(), flowConfig(t, f, "stub"), &cliFlags{}, reg) })
	if err == nil || err.Error() != "OAuth error: access_denied" {
		t.Fatalf("err = %v", err)
	}

	opened := false
	launchBrowser = func(string) error { opened = true; return nil }
	stub.authURL = "http://%zz"
	captureStdout(t, func() { err = executeOAuthFlow(context.Background(), flowConfig(t, f, "stub"), &cliFlags{}, reg) })
	if err == nil || !strings.HasPrefix(err.Error(), "failed to build authorization URL: ") || opened {
		t.Fatalf("err = %v, browser opened %v", err, opened)
	}
}
