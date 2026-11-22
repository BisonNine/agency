package skills

import (
	"github.com/spf13/cobra"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/skills"
)

func newRemoveCommand(installerFn func() (*skills.SkillInstaller, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove",
		Aliases: []string{"rm", "uninstall"},
		Short:   "Remove installed skill",
		Args:    cobra.ExactArgs(1),
		Example: app.BinaryName() + " skills remove weather",
		RunE: func(_ *cobra.Command, args []string) error {
			installer, err := installerFn()
			if err != nil {
				return err
			}
			skillsRemoveCmd(installer, args[0])
			return nil
		},
	}

	return cmd
}
