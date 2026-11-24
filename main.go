// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package main

import (
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
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
