package commands

import (
	"context"
	"fmt"

	"go.bison9.com/agency/app"
)

func versionCommand() Definition {
	return Definition{
		Name:        "version",
		Description: "Show version and copyright information",
		Usage:       "/version",
		Handler: func(_ context.Context, req Request, _ *Runtime) error {
			msg := fmt.Sprintf("%s %s\n%s\n%s",
				app.Name(),
				app.Version(),
				app.TagLine(),
				app.Copyright(),
			)
			return req.Reply(msg)
		},
	}
}
