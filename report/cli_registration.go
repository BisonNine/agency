// Agency
// License: MIT

package report

import (
	"strings"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/internal/mcpreg"
)

// itemToolServer is the item of the per-CLI rows saying whether the tool
// server is registered in that CLI: "Tool server (Claude CLI)".
const itemToolServer = "Tool server"

// cliProvidersLink is where the Register tool server button is;
// mcpHostLink is where the MCP host is turned on.
const (
	cliProvidersLink = "/providers#cli-agents"
	mcpHostLink      = "/mcp/config"
)

// toolServerOff is the status of a row whose CLI cannot be registered
// because the MCP host is off; it links to mcpHostLink.
const toolServerOff = "Tool server is off."

// EnabledCLIProtocols lists, in catalogue order, the CLIs that an enabled
// model runs on: the ones Check Up has a tool server row for.
func EnabledCLIProtocols(cfg *config.Config) []string {
	used := map[string]bool{}
	protocolOf := map[string]string{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if a := modelspec.CLIAgentByProtocol(p.Protocol); a != nil && modelspec.IsCLIProtocol(p.Protocol) {
			protocolOf[p.Name] = a.Protocol
		}
	}
	for i := range cfg.Models {
		if m := &cfg.Models[i]; m.Enabled {
			if proto, ok := protocolOf[m.Provider]; ok {
				used[proto] = true
			}
		}
	}
	var out []string
	for _, a := range modelspec.CLIAgents {
		if used[a.Protocol] {
			out = append(out, a.Protocol)
		}
	}
	return out
}

// cliRegistrationRows is one row per CLI an enabled model runs on: whether
// the tool server is registered in it. The state comes from
// env.CLIRegistrations (read by the caller); a CLI missing there is "Not
// checked". A configuration state: marked for action when the CLI's models
// cannot reach the tool server.
func cliRegistrationRows(cfg *config.Config, env Environment) []assessRow {
	hostOn := mcpreg.InternalURL(cfg) != ""
	protocols := EnabledCLIProtocols(cfg)
	rows := make([]assessRow, 0, len(protocols))
	for _, proto := range protocols {
		a := modelspec.CLIAgentByProtocol(proto)
		st := mcpreg.Status{Protocol: proto, Label: a.Label, State: mcpreg.NotChecked}
		for _, s := range env.CLIRegistrations {
			if s.Protocol == proto {
				st = s
			}
		}
		if !hostOn {
			st.State = mcpreg.HostOff
		}
		action, status := toolServerStatus(st)
		rows = append(rows, assessRow{action, itemToolServer + " (" + a.Label + ")", status})
	}
	return rows
}

// toolServerStatus is a CLI's state as the row's sentence.
func toolServerStatus(st mcpreg.Status) (action bool, status string) {
	switch st.State {
	case mcpreg.Registered:
		return false, "Registered."
	case mcpreg.NotRegistered:
		return true, "Not registered."
	case mcpreg.OtherAddress:
		return true, "Registered to a different address: " + st.Address + "."
	case mcpreg.NotInstalled:
		return true, "CLI not installed."
	case mcpreg.HostOff:
		return true, toolServerOff
	default:
		return false, "Not checked."
	}
}

// toolServerLink is the WebUI location a tool server row is acted on: the MCP
// host's settings while it is off, else the CLI providers list.
func toolServerLink(item, status string) string {
	if !strings.HasPrefix(item, itemToolServer+" (") {
		return ""
	}
	if status == toolServerOff {
		return mcpHostLink
	}
	return cliProvidersLink
}
