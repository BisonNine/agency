package config

import (
	"strings"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/logger"
)

// CLI agents — the local binaries Agency can drive as providers. The catalogue
// is modelspec's (modelspec.CLIAgents): the WebUI builds its CLI section from
// it, the provider factory takes the required arguments from it, and the
// seeded config is generated to match.
//
// The permission-bypass flag is the one an operator decides on: it lets the
// CLI run commands and edit files anywhere the service user can, outside
// Agency's own controls. It is passed only when the provider's
// bypass_restrictions ("Allow CLI to bypass restrictions" in the WebUI) is on.

// CLIArgs returns the arguments to run a CLI model with (modelspec.CLIArgs):
// the protocol's bypass flags when bypass is on, its required arguments, then
// the model's own extra_args.
//
// When bypass is off, a bypass flag written into a model's extra_args is
// stripped and one warning logged: the provider setting is the one place the
// decision is made, and a flag left over from a config that predates it must
// not quietly override the checkbox.
func CLIArgs(protocol string, bypass bool, extraArgs []string) []string {
	args, ignored := modelspec.CLIArgs(protocol, bypass, extraArgs)
	if len(ignored) > 0 {
		WarnIgnoredBypassArgs(modelspec.CLIAgentByProtocol(protocol).Protocol, ignored)
	}
	return args
}

// WarnIgnoredBypassArgs logs the permission-bypass flags of a model's
// extra_args that are not passed because the provider's bypass_restrictions
// is off. It logs nothing for no flags.
func WarnIgnoredBypassArgs(protocol string, flags []string) {
	if len(flags) == 0 {
		return
	}
	// Not an alert: a configuration state. The Agents page shows it under
	// the model chain and the Check Up page lists the provider setting.
	logger.WarnCF("config", "ignoring permission-bypass flag in extra_args: Allow CLI to bypass restrictions is off for this provider; tick it in the WebUI (or set bypass_restrictions) to pass it",
		map[string]any{"protocol": protocol, "flags": strings.Join(flags, " ")})
}
