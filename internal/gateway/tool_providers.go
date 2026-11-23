package gateway

import (
	"go.bison9.com/agency/tools"
	"go.bison9.com/agency/tools/agents"
	ccv3tools "go.bison9.com/agency/tools/ccv3"
	toolsclassifier "go.bison9.com/agency/tools/classifier"
	cogmem "go.bison9.com/agency/tools/cogmem"
	common "go.bison9.com/agency/tools/common"
	"go.bison9.com/agency/tools/files"
	toolsforum "go.bison9.com/agency/tools/forum"
	fusion "go.bison9.com/agency/tools/fusion"
	toolsknowledge "go.bison9.com/agency/tools/knowledge"
	maestro "go.bison9.com/agency/tools/maestro"
	"go.bison9.com/agency/tools/msg"
	"go.bison9.com/agency/tools/schedule"
	"go.bison9.com/agency/tools/session"
	"go.bison9.com/agency/tools/shell"
	"go.bison9.com/agency/tools/skills"
	"go.bison9.com/agency/tools/timetool"
	toolsweb "go.bison9.com/agency/tools/web"
)

// registerToolProviders registers all tool providers in the global registry.
// Must be called from setupAndStartServices before agent loop initialization.
func registerToolProviders() {
	tools.RegisterProvider(tools.NamespacedProvider("file", files.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("web", toolsweb.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("session", session.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("shell", shell.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("skill", skills.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("agent", agents.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("msg", msg.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("cogmem", cogmem.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("common", common.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("time", timetool.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("maestro", maestro.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider(toolsforum.Suite, toolsforum.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider(toolsclassifier.Namespace, toolsclassifier.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider(toolsknowledge.Namespace, toolsknowledge.GlobalProvider))
	// Bare names: fusion tool names are already service-prefixed (e.g.
	// microsoft365_mail_read_inbox), so publish them without a "fusion_" prefix.
	// "fusion" still identifies the per-agent suite toggle. Service configs must
	// keep their tool names globally unique.
	tools.RegisterProvider(tools.BareNamespacedProvider("fusion", fusion.GlobalProvider))
	tools.RegisterProvider(tools.BareNamespacedProvider(ccv3tools.Suite, ccv3tools.GlobalProvider))
	// schedule stays catalogue-only: the cron tool is a runtime tool registered
	// directly via agentLoop.RegisterTool (renamed to cron_schedule).
	tools.RegisterProvider(schedule.Provider)
}

// RegisterToolProvidersForTest is exported for use in tests that need
// providers registered without starting the full gateway.
// Safe to call multiple times — RegisterProvider is idempotent.
func RegisterToolProvidersForTest() {
	registerToolProviders()
}
