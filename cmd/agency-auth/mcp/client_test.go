package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/cmd/agency-auth/providers"
)

// recorded is one request the fake MCPFusion server received.
type recorded struct {
	method, path, auth, contentType string
	body                            map[string]any
}

// fusion is a fake MCPFusion server answering each path with a canned
// status and body.
type fusion struct {
	mu      sync.Mutex
	reqs    []recorded
	answers map[string]struct {
		status int
		body   string
	}
}

func newFusion(t *testing.T) (*fusion, *Client) {
	t.Helper()
	f := &fusion{answers: map[string]struct {
		status int
		body   string
	}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		rec := recorded{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Error(err)
			}
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		a, ok := f.answers[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(a.status)
		if _, err := w.Write([]byte(a.body)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL, "tok-alice")
}

func (f *fusion) answer(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = struct {
		status int
		body   string
	}{status, body}
}

func (f *fusion) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no request received")
	}
	return f.reqs[len(f.reqs)-1]
}

func TestPing(t *testing.T) {
	f, c := newFusion(t)
	f.answer("/ping", http.StatusOK, `{"success":true,"tenant_id":"t1","timestamp":5}`)
	resp, err := c.Ping(context.Background())
	if err != nil || !resp.Success || resp.TenantID != "t1" || resp.Timestamp != 5 {
		t.Fatalf("Ping = %+v %v", resp, err)
	}
	if r := f.last(t); r.method != http.MethodGet || r.auth != "Bearer tok-alice" {
		t.Fatalf("request = %+v", r)
	}

	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, "", "authentication failed: invalid API token"},
		{http.StatusInternalServerError, "down", "ping failed with status 500: down"},
		{http.StatusOK, "not json", "failed to decode ping response"},
	} {
		f.answer("/ping", tc.status, tc.body)
		if _, err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: err = %v, want %q", tc.status, err, tc.want)
		}
	}
}

// StoreTokens posts the tokens and metadata as JSON and reports the
// server's own error message on a failure.
func TestStoreTokens(t *testing.T) {
	f, c := newFusion(t)
	f.answer("/api/v1/oauth/tokens", http.StatusCreated, `{"success":true,"token_id":"id9"}`)
	resp, err := c.StoreTokens(context.Background(), "google", "at", "rt", 3600, map[string]string{"k": "v"})
	if err != nil || resp.TokenID != "id9" {
		t.Fatalf("StoreTokens = %+v %v", resp, err)
	}
	r := f.last(t)
	if r.method != http.MethodPost || r.contentType != "application/json" || r.auth != "Bearer tok-alice" ||
		r.body["service"] != "google" || r.body["access_token"] != "at" || r.body["refresh_token"] != "rt" ||
		r.body["expires_in"] != float64(3600) {
		t.Fatalf("request = %+v", r)
	}

	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"bad service"}}`, "token storage failed with status 400: bad service"},
		{http.StatusBadRequest, `{"message":"try again"}`, "token storage failed with status 400: try again"},
		{http.StatusBadRequest, `{}`, "token storage failed with status 400: unknown error"},
		{http.StatusOK, `nope`, "failed to decode response"},
	} {
		f.answer("/api/v1/oauth/tokens", tc.status, tc.body)
		if _, err := c.StoreTokens(context.Background(), "google", "at", "", 0, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}

func TestHealthAndAuthCheck(t *testing.T) {
	f, c := newFusion(t)
	f.answer("/health", http.StatusOK, "")
	f.answer("/api/v1/auth/verify", http.StatusOK, "")
	if err := c.HealthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.AuthCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.answer("/health", http.StatusServiceUnavailable, "")
	if err := c.HealthCheck(context.Background()); err == nil || err.Error() != "MCPFusion server health check failed with status: 503" {
		t.Fatalf("health = %v", err)
	}
	f.answer("/api/v1/auth/verify", http.StatusUnauthorized, "")
	if err := c.AuthCheck(context.Background()); err == nil || err.Error() != "API token is invalid or expired" {
		t.Fatalf("auth 401 = %v", err)
	}
	f.answer("/api/v1/auth/verify", http.StatusForbidden, "")
	if err := c.AuthCheck(context.Background()); err == nil || err.Error() != "auth check failed with status: 403" {
		t.Fatalf("auth 403 = %v", err)
	}
}

func TestGetServiceConfig(t *testing.T) {
	f, c := newFusion(t)
	f.answer("/api/v1/services/google/config", http.StatusOK,
		`{"success":true,"config":{"auth_type":"oauth2","client_id":"cid","scopes":"s1 s2"}}`)
	resp, err := c.GetServiceConfig(context.Background(), "google")
	if err != nil || resp.Config.ClientID != "cid" || resp.Config.Scopes != "s1 s2" {
		t.Fatalf("GetServiceConfig = %+v %v", resp, err)
	}
	if at, ok := resp.Config.AuthType(); !ok || at != "oauth2" {
		t.Fatalf("AuthType = %q %v", at, ok)
	}
	f.answer("/api/v1/services/google/config", http.StatusNotFound, `{"message":"no such service"}`)
	if _, err := c.GetServiceConfig(context.Background(), "google"); err == nil ||
		err.Error() != "service config request failed with status 404: no such service" {
		t.Fatalf("404 = %v", err)
	}
	f.answer("/api/v1/services/google/config", http.StatusOK, `x`)
	if _, err := c.GetServiceConfig(context.Background(), "google"); err == nil || !strings.Contains(err.Error(), "failed to decode service config response") {
		t.Fatalf("bad body = %v", err)
	}
}

func TestAuthType_Absent(t *testing.T) {
	var nilData *ServiceConfigData
	if _, ok := nilData.AuthType(); ok {
		t.Fatal("nil config has no auth type")
	}
	if _, ok := (&ServiceConfigData{}).AuthType(); ok {
		t.Fatal("empty auth type is absent")
	}
}

// The notifications post the service and what happened; an error
// notification never fails the caller.
func TestNotify(t *testing.T) {
	f, c := newFusion(t)
	f.answer("/api/v1/oauth/success", http.StatusOK, "")
	if err := c.NotifySuccess(context.Background(), "google", &providers.UserInfo{Email: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	r := f.last(t)
	user, ok := r.body["user_info"].(map[string]any)
	if !ok || r.body["service"] != "google" || user["email"] != "alice@example.com" {
		t.Fatalf("success body = %+v", r.body)
	}
	f.answer("/api/v1/oauth/success", http.StatusBadGateway, "")
	if err := c.NotifySuccess(context.Background(), "google", nil); err == nil || err.Error() != "success notification failed with status: 502" {
		t.Fatalf("502 = %v", err)
	}

	if err := c.NotifyError(context.Background(), "google", "denied"); err != nil {
		t.Fatalf("NotifyError = %v, want nil even on a 404", err)
	}
	if r := f.last(t); r.path != "/api/v1/oauth/error" || r.body["error"] != "denied" {
		t.Fatalf("error body = %+v", r)
	}
}

// An unreachable server or an unusable URL fails every call with its own
// message.
func TestClient_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := NewClient(url, "t")
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["ping request failed"] = c.Ping(ctx)
	_, checks["failed to send token request"] = c.StoreTokens(ctx, "s", "a", "", 0, nil)
	checks["health check failed"] = c.HealthCheck(ctx)
	checks["auth check failed"] = c.AuthCheck(ctx)
	_, checks["service config request failed"] = c.GetServiceConfig(ctx, "s")
	checks["success notification failed"] = c.NotifySuccess(ctx, "s", nil)
	checks["error notification failed"] = c.NotifyError(ctx, "s", "e")
	for want, err := range checks {
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("err = %v, want prefix %q", err, want)
		}
	}

	bad := NewClient("http://%zz", "t")
	badChecks := map[string]error{}
	_, badChecks["failed to create ping request"] = bad.Ping(ctx)
	_, badChecks["failed to create HTTP request"] = bad.StoreTokens(ctx, "s", "a", "", 0, nil)
	badChecks["failed to create health check request"] = bad.HealthCheck(ctx)
	badChecks["failed to create auth check request"] = bad.AuthCheck(ctx)
	_, badChecks["failed to create service config request"] = bad.GetServiceConfig(ctx, "s")
	badChecks["failed to create success notification request"] = bad.NotifySuccess(ctx, "s", nil)
	badChecks["failed to create error notification request"] = bad.NotifyError(ctx, "s", "e")
	for want, err := range badChecks {
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("err = %v, want prefix %q", err, want)
		}
	}
}
