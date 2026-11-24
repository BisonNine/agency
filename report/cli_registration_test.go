// Agency
// License: MIT

package report

import (
	"testing"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/mcpreg"
)

func cliRegistrationConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.MCPHost.Enabled = true
	cfg.Providers = []modelspec.Provider{
		{Name: "anthropic", Protocol: "anthropic", APIKey: "k"},
		{Name: "Codex", Protocol: "codex-cli"},
		{Name: "Gem", Protocol: "gemini-cli"}, // the old alias is Antigravity
		{Name: "Claude", Protocol: "claude-cli"},
		{Name: "Cursor", Protocol: "cursor-cli"},
	}
	cfg.Models = []modelspec.Model{
		{ModelName: "api", Model: "m", Provider: "anthropic", Enabled: true},
		{ModelName: "codex", Model: "codex-cli", Provider: "Codex", Enabled: true},
		{ModelName: "gem", Model: "x", Provider: "Gem", Enabled: true},
		{ModelName: "claude", Model: "claude-cli", Provider: "Claude", Enabled: true},
		{ModelName: "claude2", Model: "opus", Provider: "Claude", Enabled: true},
		{ModelName: "cursor", Model: "cursor-cli", Provider: "Cursor", Enabled: false},
	}
	return cfg
}

func TestEnabledCLIProtocols(t *testing.T) {
	got := EnabledCLIProtocols(cliRegistrationConfig())
	want := []string{"claude-cli", "codex-cli", "antigravity-cli"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	if got := EnabledCLIProtocols(config.DefaultConfig()); len(got) != 0 {
		t.Errorf("default config: %v", got)
	}
}

// One row per CLI in use, in catalogue order, with the state the caller read;
// a CLI the caller did not read is not checked.
func TestCLIRegistrationRows(t *testing.T) {
	cfg := cliRegistrationConfig()
	env := Environment{CLIRegistrations: []mcpreg.Status{
		{Protocol: "claude-cli", State: mcpreg.Registered},
		{Protocol: "codex-cli", State: mcpreg.OtherAddress, Address: "http://127.0.0.1:7000/internal"},
		{Protocol: "cursor-cli", State: mcpreg.NotRegistered},
	}}
	got := Assess(t.Context(), cfg, env).Assessment
	want := []AssessmentRow{
		{false, "Tool server (Claude CLI)", "Registered.", cliProvidersLink},
		{true, "Tool server (Codex CLI)", "Registered to a different address: http://127.0.0.1:7000/internal.", cliProvidersLink},
		{false, "Tool server (Antigravity CLI)", "Not checked.", cliProvidersLink},
	}
	var rows []AssessmentRow
	for _, r := range got {
		if len(r.Item) > len(itemToolServer) && r.Item[:len(itemToolServer)] == itemToolServer {
			rows = append(rows, r)
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// With the MCP host off every row says so and links to its setting.
	cfg.MCPHost.Enabled = false
	cfg.MCPHost.AutoEnable = false
	for _, r := range cliRegistrationRows(cfg, env) {
		if r.status != toolServerOff || !r.action {
			t.Errorf("host off: %+v", r)
		}
		if link := rowLink(r.item, r.status); link != mcpHostLink {
			t.Errorf("host off link %q", link)
		}
	}
}

func TestToolServerStatus(t *testing.T) {
	cases := map[mcpreg.State]struct {
		action bool
		status string
	}{
		mcpreg.Registered:    {false, "Registered."},
		mcpreg.NotRegistered: {true, "Not registered."},
		mcpreg.NotInstalled:  {true, "CLI not installed."},
		mcpreg.HostOff:       {true, toolServerOff},
		mcpreg.NotChecked:    {false, "Not checked."},
	}
	for state, want := range cases {
		action, status := toolServerStatus(mcpreg.Status{State: state})
		if action != want.action || status != want.status {
			t.Errorf("%s: %v %q", state, action, status)
		}
	}
	if link := toolServerLink("Shell access", "x"); link != "" {
		t.Errorf("other row linked: %q", link)
	}
}
