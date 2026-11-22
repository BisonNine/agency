package status

import (
	"github.com/spf13/cobra"

	"go.bison9.com/agency/app"
)

func NewStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"s"},
		Short:   "Show " + app.ShortName() + " status",
		Run: func(cmd *cobra.Command, args []string) {
			statusCmd()
		},
	}

	return cmd
}
