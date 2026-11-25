// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/internal/admincmd"
	"go.bison9.com/agency/internal/agent"
	"go.bison9.com/agency/internal/backup"
	"go.bison9.com/agency/internal/cron"
	"go.bison9.com/agency/internal/devicegw"
	"go.bison9.com/agency/internal/gateway"
	"go.bison9.com/agency/internal/install"
	"go.bison9.com/agency/internal/memory"
	"go.bison9.com/agency/internal/model"
	"go.bison9.com/agency/internal/network"
	"go.bison9.com/agency/internal/runas"
	"go.bison9.com/agency/internal/sessions"
	"go.bison9.com/agency/internal/skills"
	"go.bison9.com/agency/internal/status"
	"go.bison9.com/agency/internal/test"
	"go.bison9.com/agency/internal/tlscert"
	"go.bison9.com/agency/internal/token"
	"go.bison9.com/agency/internal/upgrade"
	"go.bison9.com/agency/internal/version"
)

func NewAgencyCommand() *cobra.Command {
	// Default subcommand: run the merged gateway + WebUI + session API on the
	// single port from cfg.Gateway. `agency` with no arguments is the supported
	// way to launch the server; the `agency gateway` alias is preserved for
	// existing systemd units and muscle memory during the transition.
	defaultCmd := gateway.NewGatewayCommand()
	cmd := &cobra.Command{
		Use:          app.BinaryName(),
		Short:        app.TagLine(),
		Args:         defaultCmd.Args,
		SilenceUsage: true,
		PreRunE:      defaultCmd.PreRunE,
		RunE:         defaultCmd.RunE,
	}
	cmd.Flags().AddFlagSet(defaultCmd.Flags())

	cmd.AddCommand(
		admincmd.NewAdminCommand(),
		agent.NewAgentCommand(),
		defaultCmd,
		gateway.NewACPCommand(),
		status.NewStatusCommand(),
		cron.NewCronCommand(),
		skills.NewSkillsCommand(),
		memory.NewMemoryCommand(),
		sessions.NewSessionsCommand(),
		model.NewModelCommand(),
		install.NewInstallCommand(),
		install.NewUninstallCommand(),
		network.NewNetworkCommand(),
		test.NewTestCommand(),
		token.NewTokenCommand(),
		tlscert.NewTLSCommand(),
		devicegw.NewDevicesCommand(),
		backup.NewBackupCommand(),
		backup.NewRestoreCommand(),
		upgrade.NewUpgradeCommand(),
		version.NewVersionCommand(),
	)

	return cmd
}

func main() {
	cmd := NewAgencyCommand()
	acct, err := runas.Drop(topLevelCommand(cmd, os.Args[1:]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if acct != nil {
		// The error is printed here, so a path the account cannot reach
		// is named in the operator's words (runas.Explain).
		cmd.SilenceErrors = true
	}
	if err := cmd.Execute(); err != nil {
		if acct != nil {
			if explained := runas.Explain(err, acct); explained != err { //nolint:errorlint // identity: was it rewritten
				fmt.Fprintln(os.Stderr, explained)
			} else {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		}
		os.Exit(1)
	}
}

// topLevelCommand is the name of the root's subcommand args run ("" for the
// root itself, the service), so a root process can switch to the service
// account before any command touches the data directory.
func topLevelCommand(root *cobra.Command, args []string) string {
	// Execute adds these itself; Find only sees them once they are there.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(args...)
	c, _, err := root.Find(args)
	if err != nil {
		// An unknown command fails in Execute, so it never runs.
		return "help"
	}
	for c.HasParent() && c.Parent() != root {
		c = c.Parent()
	}
	if c == root {
		return ""
	}
	return c.Name()
}
