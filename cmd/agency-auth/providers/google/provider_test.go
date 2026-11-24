package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agency/cmd/agency-auth/providers"
)

// roundTrip answers every request from fn, so the provider's fixed Google
// URLs are never dialled.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func withTransport(fn roundTrip) *Provider {
	p := NewProvider()
	p.httpClient = &http.Client{Transport: fn}
	return p
}

func TestProvider_Descriptors(t *testing.T) {
	p := NewProvider()
	if p.GetServiceName() != "google" || p.GetDisplayName() != "Google APIs" ||
		p.SupportsDeviceFlow() || !p.SupportsAuthorizationCode() {
		t.Fatal("descriptor mismatch")
	}
	if !strings.HasPrefix(p.GetAuthorizationEndpoint(), "https://accounts.google.com/") ||
		!strings.HasPrefix(p.GetTokenEndpoint(), "https://oauth2.googleapis.com/") ||
		!strings.HasPrefix(p.GetDeviceCodeEndpoint(), "https://oauth2.googleapis.com/") {
		t.Fatal("endpoint mismatch")
	}
	for _, s := range p.GetRequiredScopes() {
		if !strings.HasPrefix(s, "https://www.googleapis.com/auth/") {
			t.Errorf("scope %q", s)
		}
	}
	if ext := p.GetExtendedScopes(); len(ext["gmail"]) == 0 || len(ext["drive"]) == 0 {
		t.Fatalf("extended scopes = %v", ext)
	}
}

func TestValidateConfiguration(t *testing.T) {
	p := NewProvider()
	for _, tc := range []struct {
		cfg  providers.ServiceConfig
		want string
	}{
		{providers.ServiceConfig{}, "client_id is required for Google OAuth"},
		{providers.ServiceConfig{ClientID: "c", Scopes: "https://www.googleapis.com/auth/drive email"}, "invalid Google scope format: email"},
		{providers.ServiceConfig{ClientID: "c", Scopes: "https://www.googleapis.com/auth/drive"}, ""},
		{providers.ServiceConfig{ClientID: "c"}, ""},
	} {
		err := p.ValidateConfiguration(&tc.cfg)
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || err.Error() != tc.want)) {
			t.Errorf("%+v: err = %v, want %q", tc.cfg, err, tc.want)
		}
	}
}

func TestCustomizeRequests(t *testing.T) {
	p := NewProvider()
	params := map[string]string{}
	if err := p.CustomizeDeviceRequest(params, &providers.ServiceConfig{ClientID: "c"}); err != nil {
		t.Fatal(err)
	}
	if params["client_id"] != "c" || params["response_type"] != "device_code" || params["scope"] != strings.Join(p.GetRequiredScopes(), " ") {
		t.Fatalf("device params = %v", params)
	}
	params = map[string]string{}
	if err := p.CustomizeDeviceRequest(params, &providers.ServiceConfig{ClientID: "c", Scopes: "x"}); err != nil {
		t.Fatal(err)
	}
	if params["scope"] != "x" {
		t.Fatalf("configured scope not used: %v", params)
	}

	params = map[string]string{}
	if err := p.CustomizeTokenRequest(params, &providers.ServiceConfig{ClientID: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := params["client_secret"]; ok || params["client_id"] != "c" {
		t.Fatalf("token params without secret = %v", params)
	}
	if err := p.CustomizeTokenRequest(params, &providers.ServiceConfig{ClientID: "c", ClientSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if params["client_secret"] != "s" {
		t.Fatalf("token params = %v", params)
	}
}

func TestProcessTokenResponse(t *testing.T) {
	p := NewProvider()
	if _, err := p.ProcessTokenResponse(map[string]any{}); err == nil || err.Error() != "access_token not found in response" {
		t.Fatalf("missing token: %v", err)
	}
	before := time.Now()
	tok, err := p.ProcessTokenResponse(map[string]any{
		"access_token": "at", "refresh_token": "rt", "token_type": "MAC",
		"expires_in": float64(60), "scope": "a b", "id_token": "idt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.TokenType != "MAC" || tok.ExpiresIn != 60 ||
		len(tok.Scope) != 2 || tok.IDToken != "idt" || tok.ExpiresAt == nil || tok.ExpiresAt.Before(before.Add(59*time.Second)) {
		t.Fatalf("token = %+v", tok)
	}
	if tok, err = p.ProcessTokenResponse(map[string]any{"access_token": "at"}); err != nil {
		t.Fatal(err)
	}
	if tok.TokenType != "Bearer" || tok.ExpiresAt != nil {
		t.Fatalf("defaults = %+v", tok)
	}
}

func TestGetUserInfo(t *testing.T) {
	var gotAuth, gotURL string
	p := withTransport(func(r *http.Request) (*http.Response, error) {
		gotAuth, gotURL = r.Header.Get("Authorization"), r.URL.String()
		return respond(http.StatusOK, `{"id":"1","email":"alice@example.com","name":"Alice"}`), nil
	})
	u, err := p.GetUserInfo(context.Background(), &providers.TokenInfo{AccessToken: "at"})
	if err != nil || u.Email != "alice@example.com" || u.DisplayName != "Alice" || u.ID != "1" {
		t.Fatalf("user = %+v %v", u, err)
	}
	if gotAuth != "Bearer at" || gotURL != "https://www.googleapis.com/oauth2/v2/userinfo" {
		t.Fatalf("request auth %q url %q", gotAuth, gotURL)
	}

	for _, tc := range []struct {
		rt   roundTrip
		want string
	}{
		{func(*http.Request) (*http.Response, error) { return respond(http.StatusUnauthorized, ""), nil }, "user info request failed with status: 401"},
		{func(*http.Request) (*http.Response, error) { return respond(http.StatusOK, "x"), nil }, "failed to decode user info response"},
		{func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }, "failed to get user info"},
	} {
		if _, err := withTransport(tc.rt).GetUserInfo(context.Background(), &providers.TokenInfo{AccessToken: "at"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
	//nolint:staticcheck // a nil context is what makes the request fail to build
	if _, err := p.GetUserInfo(nil, &providers.TokenInfo{}); err == nil || !strings.Contains(err.Error(), "failed to create user info request") {
		t.Errorf("nil context: %v", err)
	}
}

func TestValidateToken(t *testing.T) {
	var gotBody string
	p := withTransport(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		gotBody = string(b)
		return respond(http.StatusOK, `{"aud":"a","azp":"c","expires_in":"60","scope":"s"}`), nil
	})
	if err := p.ValidateToken(context.Background(), &providers.TokenInfo{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if gotBody != "access_token=at" {
		t.Fatalf("body = %q", gotBody)
	}
	for _, tc := range []struct {
		rt   roundTrip
		want string
	}{
		{func(*http.Request) (*http.Response, error) { return respond(http.StatusBadRequest, ""), nil }, "token validation failed with status: 400"},
		{func(*http.Request) (*http.Response, error) { return respond(http.StatusOK, "x"), nil }, "failed to decode token info response"},
		{func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }, "failed to validate token"},
	} {
		if err := withTransport(tc.rt).ValidateToken(context.Background(), &providers.TokenInfo{AccessToken: "at"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
	//nolint:staticcheck // a nil context is what makes the request fail to build
	if err := p.ValidateToken(nil, &providers.TokenInfo{}); err == nil || !strings.Contains(err.Error(), "failed to create token validation request") {
		t.Errorf("nil context: %v", err)
	}
}
