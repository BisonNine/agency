package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.bison9.com/agency/app"
)

func TestNewAgencyCommand(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"/usr/local/bin/alice-agency"}

	cmd := NewAgencyCommand()

	require.NotNil(t, cmd)

	assert.Equal(t, "alice-agency", cmd.Use, "the root command is named after the binary as started")
	assert.Equal(t, app.TagLine(), cmd.Short)

	assert.True(t, cmd.HasSubCommands())
	assert.True(t, cmd.HasAvailableSubCommands())

	// The merged binary attaches the gateway flags (-d / -T) on the root so
	// `agency` (no subcommand) launches the server.
	assert.True(t, cmd.HasFlags())

	assert.Nil(t, cmd.Run)
	// `agency` with no args launches the gateway, so RunE must be set.
	assert.NotNil(t, cmd.RunE)

	assert.Nil(t, cmd.PersistentPreRun)
	assert.Nil(t, cmd.PersistentPostRun)

	allowedCommands := []string{
		"acp",
		"admin",
		"agent",
		"backup",
		"cron",
		"devices",
		"gateway",
		"install",
		"memory",
		"test",
		"model",
		"network",
		"restore",
		"sessions",
		"skills",
		"status",
		"tls",
		"token",
		"uninstall",
		"upgrade",
		"version",
	}

	subcommands := cmd.Commands()
	assert.Len(t, subcommands, len(allowedCommands))

	for _, subcmd := range subcommands {
		found := slices.Contains(allowedCommands, subcmd.Name())
		assert.True(t, found, "unexpected subcommand %q", subcmd.Name())

		assert.False(t, subcmd.Hidden)
	}
}

func TestUpdateAlias(t *testing.T) {
	cmd := NewAgencyCommand()
	subcmd, _, err := cmd.Find([]string{"update"})
	require.NoError(t, err)
	require.NotNil(t, subcmd)
	assert.Equal(t, "upgrade", subcmd.Name())
}

// TestTopLevelCommand: the privilege switch is decided by the top-level
// command a command line runs ("" for the service itself).
func TestTopLevelCommand(t *testing.T) {
	root := NewAgencyCommand()
	for args, want := range map[string]string{
		"":                    "",
		"--debug":             "",
		"admin alice":         "admin",
		"cron list":           "cron",
		"install --package":   "install",
		"uninstall --package": "uninstall",
		"upgrade --check":     "upgrade",
		"help admin":          "help",
		"completion bash":     "completion",
	} {
		if got := topLevelCommand(root, strings.Fields(args)); got != want {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
}
