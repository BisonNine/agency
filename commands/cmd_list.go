package commands

import (
	"context"
	"strings"
)

func listCommand() Definition {
	return Definition{
		Name:        "list",
		Description: "List available options",
		SubCommands: []SubCommand{
			{
				Name:        "models",
				Description: "Configured models",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.GetAgentModels == nil {
						return req.Reply(noModelText(rt))
					}
					entries, active := rt.GetAgentModels()
					if len(entries) == 0 {
						return req.Reply(noModelText(rt))
					}
					return req.Reply(renderModelList(entries, active))
				},
			},
			{
				Name:        "channels",
				Description: "Enabled channels",
				Handler: func(_ context.Context, req Request, rt *Runtime) error {
					if rt == nil || rt.GetEnabledChannels == nil {
						return req.Reply(unavailableMsg)
					}
					enabled := rt.GetEnabledChannels()
					if len(enabled) == 0 {
						return req.Reply("No channels enabled")
					}
					return req.Reply("Enabled Channels:\n- " + strings.Join(enabled, "\n- "))
				},
			},
			{
				Name:        "agents",
				Description: "Registered agents",
				Handler:     agentsHandler(),
			},
		},
	}
}
