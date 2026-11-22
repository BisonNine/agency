package config

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agency/logger"
)

// Stripping a flag silently would leave an operator wondering why the CLI
// refuses tools when the config plainly lists the flag. One warning, naming the
// checkbox, per invocation.
func TestCLIArgs_WarnsOnceWhenStrippingABypassFlag(t *testing.T) {
	var buf bytes.Buffer
	restore := logger.RedirectForTest(&buf)
	defer restore()

	CLIArgs("claude-cli", false, []string{"--dangerously-skip-permissions", "--verbose"})
	out := buf.String()
	if !strings.Contains(out, "Allow CLI to bypass restrictions") || !strings.Contains(out, "--dangerously-skip-permissions") {
		t.Errorf("no warning naming the checkbox and the flag:\n%s", out)
	}
	if strings.Count(out, "Allow CLI to bypass restrictions") != 1 {
		t.Errorf("want exactly one warning:\n%s", out)
	}

	buf.Reset()
	CLIArgs("claude-cli", true, []string{"--dangerously-skip-permissions"})
	CLIArgs("claude-cli", false, []string{"--verbose"})
	if buf.Len() != 0 {
		t.Errorf("warned with nothing to strip:\n%s", buf.String())
	}
}

// The setting defaults off: a provider that says nothing about it runs the CLI
// under the CLI's own permissions.
func TestProvider_BypassRestrictionsDefaultsOff(t *testing.T) {
	cfg := DefaultConfig()
	for i := range cfg.Providers {
		if p := &cfg.Providers[i]; IsCLIProtocol(p.Protocol) && p.BypassRestrictions {
			t.Errorf("seeded provider %q has bypass_restrictions on", p.Name)
		}
	}
	if (Provider{}).BypassRestrictions {
		t.Error("zero-value provider has bypass_restrictions on")
	}
}

// The seeded models carry no flags of their own: the catalogue supplies them,
// so a fresh install and a toggled-on CLI run with the same command line, and
// no seeded model smuggles in a permission-bypass flag the checkbox is off for.
func TestSeededCLIModelsMatchTheCatalogue(t *testing.T) {
	cfg := DefaultConfig()
	byName := map[string]*Provider{}
	for i := range cfg.Providers {
		byName[cfg.Providers[i].Name] = &cfg.Providers[i]
	}
	seen := map[string]bool{}
	for i := range cfg.Models {
		m := &cfg.Models[i]
		prov := byName[m.Provider]
		if prov == nil || !IsCLIProtocol(prov.Protocol) {
			continue
		}
		agent := CLIAgentByProtocol(prov.Protocol)
		if agent == nil {
			t.Errorf("seeded model %q uses CLI protocol %q with no catalogue row", m.ModelName, prov.Protocol)
			continue
		}
		if m.Model == agent.SentinelModel() {
			seen[prov.Protocol] = true
		}
		if len(m.ExtraArgs) != 0 {
			t.Errorf("seeded model %q carries extra_args %v; the catalogue supplies a CLI's flags",
				m.ModelName, m.ExtraArgs)
		}
		if got := CLIArgs(prov.Protocol, prov.BypassRestrictions, m.ExtraArgs); !slices.Equal(got, agent.RequiredArgs) {
			t.Errorf("seeded model %q runs with %v, want the catalogue's required args %v and no bypass flag",
				m.ModelName, got, agent.RequiredArgs)
		}
		if m.RequestTimeout != agent.RequestTimeout {
			t.Errorf("seeded model %q timeout = %d, catalogue says %d",
				m.ModelName, m.RequestTimeout, agent.RequestTimeout)
		}
	}
	for _, c := range CLIAgents {
		if !seen[c.Protocol] {
			t.Errorf("no seeded sentinel model for %s", c.Protocol)
		}
	}
}
