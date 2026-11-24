package config

import (
	"testing"
	"time"
)

// Service values win; empty ones take the global value; the stored service
// entry is not changed by the merge.
func TestMergeServiceConfig(t *testing.T) {
	c := &Config{ClientID: "global-id", ClientSecret: "global-secret", TenantID: "global-tenant", RedirectURI: "http://global/cb"}
	c.SetServiceConfig("google", &ServiceConfig{DisplayName: "Google", ClientID: "svc-id", Scope: "s1"})

	got := c.MergeServiceConfig("google")
	if got.ClientID != "svc-id" || got.ClientSecret != "global-secret" || got.TenantID != "global-tenant" ||
		got.RedirectURI != "http://global/cb" || got.DisplayName != "Google" || got.Scope != "s1" {
		t.Fatalf("merged = %+v", got)
	}
	if stored := c.GetServiceConfig("google"); stored.ClientSecret != "" {
		t.Fatal("merge changed the stored service entry")
	}

	got = c.MergeServiceConfig("unknown")
	if got.ClientID != "global-id" || got.DisplayName != "" {
		t.Fatalf("unknown service merged = %+v", got)
	}
}

func TestGetServiceConfig_NoServices(t *testing.T) {
	if (&Config{}).GetServiceConfig("google") != nil {
		t.Fatal("want nil with no services")
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Service: "google", FusionURL: "http://f", APIToken: "t", Timeout: time.Minute}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for _, tc := range []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) { c.Service = "" }, "service is required"},
		{func(c *Config) { c.FusionURL = "" }, "fusion URL is required"},
		{func(c *Config) { c.APIToken = "" }, "API token is required"},
		{func(c *Config) { c.Timeout = 0 }, "timeout must be positive"},
	} {
		c := ok
		tc.mutate(&c)
		if err := c.Validate(); err == nil || err.Error() != tc.want {
			t.Errorf("Validate() = %v, want %q", err, tc.want)
		}
	}
}

// Defaults fill only the services the config does not already have.
func TestLoadServiceDefaults(t *testing.T) {
	c := &Config{}
	c.LoadServiceDefaults()
	for _, name := range []string{"google", "github", "microsoft365"} {
		if c.GetServiceConfig(name) == nil {
			t.Errorf("default %s missing", name)
		}
	}
	if got := c.GetServiceConfig("google").Endpoints.TokenURL; got != "https://oauth2.googleapis.com/token" {
		t.Errorf("google token URL = %q", got)
	}

	c = &Config{}
	c.SetServiceConfig("google", &ServiceConfig{ClientID: "mine"})
	c.LoadServiceDefaults()
	if got := c.GetServiceConfig("google"); got.ClientID != "mine" || got.DisplayName != "" {
		t.Fatalf("existing google entry replaced: %+v", got)
	}
}
