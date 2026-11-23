package providers

import (
	"context"
	"testing"
	"time"
)

// stubProvider is a minimal OAuthProvider.
type stubProvider struct{ name string }

func (s stubProvider) GetServiceName() string                     { return s.name }
func (s stubProvider) GetDisplayName() string                     { return "Stub " + s.name }
func (s stubProvider) GetRequiredScopes() []string                { return []string{"scope-a"} }
func (s stubProvider) GetAuthorizationEndpoint() string           { return "" }
func (s stubProvider) GetTokenEndpoint() string                   { return "" }
func (s stubProvider) GetDeviceCodeEndpoint() string              { return "" }
func (s stubProvider) SupportsDeviceFlow() bool                   { return true }
func (s stubProvider) SupportsAuthorizationCode() bool            { return false }
func (s stubProvider) ValidateConfiguration(*ServiceConfig) error { return nil }
func (s stubProvider) CustomizeDeviceRequest(map[string]string, *ServiceConfig) error {
	return nil
}
func (s stubProvider) CustomizeTokenRequest(map[string]string, *ServiceConfig) error { return nil }
func (s stubProvider) ProcessTokenResponse(map[string]any) (*TokenInfo, error)       { return nil, nil }

func (s stubProvider) GetUserInfo(context.Context, *TokenInfo) (*UserInfo, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	r := NewProviderRegistry()
	if err := r.Register(stubProvider{name: ""}); err == nil || err.Error() != "provider service name cannot be empty" {
		t.Fatalf("empty name: %v", err)
	}
	if err := r.Register(stubProvider{name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubProvider{name: "alpha"}); err == nil || err.Error() != "provider for service 'alpha' already registered" {
		t.Fatalf("duplicate: %v", err)
	}
	if p, err := r.GetProvider("alpha"); err != nil || p.GetServiceName() != "alpha" {
		t.Fatalf("GetProvider = %v %v", p, err)
	}
	if _, err := r.GetProvider("beta"); err == nil || err.Error() != "no provider registered for service 'beta'" {
		t.Fatalf("unknown: %v", err)
	}
	if names := r.ListProviders(); len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("ListProviders = %v", names)
	}
	info := r.GetAvailableProviders()["alpha"]
	if info.ServiceName != "alpha" || info.DisplayName != "Stub alpha" || !info.SupportsDeviceFlow || info.SupportsAuthCode ||
		len(info.DefaultScopes) != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestTokenInfo(t *testing.T) {
	never := &TokenInfo{AccessToken: "a"}
	if never.IsExpired() || never.IsExpiredWithBuffer(time.Hour) {
		t.Fatal("a token without expiry never expires")
	}
	if never.GetAuthorizationHeader() != "Bearer a" {
		t.Fatalf("header = %q", never.GetAuthorizationHeader())
	}
	past := time.Now().Add(-time.Minute)
	soon := time.Now().Add(time.Minute)
	if !(&TokenInfo{ExpiresAt: &past}).IsExpired() {
		t.Fatal("past expiry must be expired")
	}
	tok := &TokenInfo{AccessToken: "b", TokenType: "MAC", ExpiresAt: &soon}
	if tok.IsExpired() || !tok.IsExpiredWithBuffer(time.Hour) || tok.IsExpiredWithBuffer(time.Second) {
		t.Fatal("buffer check wrong")
	}
	if tok.GetAuthorizationHeader() != "MAC b" {
		t.Fatalf("header = %q", tok.GetAuthorizationHeader())
	}
}
