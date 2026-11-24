// Agency
// License: MIT

package agent

import (
	"os"
	"testing"

	"go.bison9.com/agency/tools"
	toolscogmem "go.bison9.com/agency/tools/cogmem"
	toolsfiles "go.bison9.com/agency/tools/files"
	toolsmaestro "go.bison9.com/agency/tools/maestro"
	toolssession "go.bison9.com/agency/tools/session"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// TestMain registers the tool providers needed by agent-package tests.
// In production, tool providers are registered by internal/gateway before
// NewAgentLoop is called. Tests that instantiate NewAgentLoop directly
// (e.g. TestAgentLoop_GetStartupInfo) need at least a few providers registered
// so that the agent builder produces a non-empty tool registry.
func TestMain(m *testing.M) {
	tools.RegisterProvider(tools.NamespacedProvider("file", toolsfiles.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("shell", toolsshell.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("session", toolssession.GlobalProvider))
	tools.RegisterProvider(tools.NamespacedProvider("cogmem", toolscogmem.GlobalProvider))
	// Only agents with the Maestro suite switched on get its tools.
	tools.RegisterProvider(tools.NamespacedProvider("maestro", toolsmaestro.GlobalProvider))
	os.Exit(m.Run())
}
