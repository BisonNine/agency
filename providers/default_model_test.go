// Agency
// License: MIT

package providers

import (
	"context"
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
)

// The default-model check reads as it always has, word for word.
func TestCheckDefaultModel_Wording(t *testing.T) {
	cfgWith := func(p modelspec.Provider) *config.Config {
		p.Name = "Claude"
		c := &config.Config{
			Providers: []modelspec.Provider{p},
			Models:    []modelspec.Model{{ModelName: "sonnet", Model: "claude-sonnet", Provider: "Claude", Enabled: true}},
		}
		c.Agents.Defaults.Models = []string{"sonnet"}
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"no models", &config.Config{}, "no models configured. Please add entries to models in your config"},
		{
			"missing key", cfgWith(modelspec.Provider{Protocol: "anthropic", BaseURL: "https://x"}),
			`failed to create provider for model "sonnet": provider "Claude": api_key required for anthropic protocol`,
		},
		{
			"task-only protocol", cfgWith(modelspec.Provider{Protocol: modelspec.GeminiProtocol, APIKey: "k"}),
			`failed to create provider for model "sonnet": provider "Claude": protocol "gemini" serves task models only (speech), not chat`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDefaultModel(context.Background(), tc.cfg)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant %s", err, tc.want)
			}
		})
	}
}
