package version

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.bison9.com/agency/app"
)

func NewVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "version",
		Aliases: []string{"v"},
		Short:   "Show version information",
		Run: func(_ *cobra.Command, _ []string) {
			printVersion()
		},
	}

	return cmd
}

// buildInfo is app.BuildInfo; a variable so tests can supply a stamped build.
var buildInfo = app.BuildInfo

func printVersion() {
	fmt.Printf("%s %s\n", app.Name(), app.Version())
	build, goVer := buildInfo()
	if build != "" {
		fmt.Printf("  Build: %s\n", build)
	}
	if goVer != "" {
		fmt.Printf("  Go: %s\n", goVer)
	}
}
