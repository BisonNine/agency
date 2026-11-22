package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func modelCommand() Definition {
	return Definition{
		Name:        "model",
		Description: "Select which configured model to use",
		Usage:       "/model <n>",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.GetAgentModels == nil || rt.SetActiveModel == nil {
				return req.Reply(noModelText(rt))
			}
			entries, active := rt.GetAgentModels()
			if len(entries) == 0 {
				return req.Reply(noModelText(rt))
			}

			arg := nthToken(req.Text, 1)
			idx, err := strconv.Atoi(arg)
			if arg == "" || err != nil {
				return req.Reply("Usage: /model <n>\n\n" + renderModelList(entries, active))
			}

			name, serr := rt.SetActiveModel(idx)
			if serr != nil {
				return req.Reply(serr.Error())
			}
			return req.Reply(fmt.Sprintf("Model set to %d: %s", idx, name))
		},
	}
}

// renderModelList renders a numbered (from 0) list of the agent's candidate
// models, marking the active entry, with a footer pointing to /model.
func renderModelList(entries []ModelEntry, active int) string {
	var b strings.Builder
	b.WriteString("Configured Models:\n")
	for i, e := range entries {
		marker := "  "
		if i == active {
			marker = "▶ "
		}
		if e.Provider != "" {
			fmt.Fprintf(&b, "%s%d: %s  (%s)\n", marker, i, e.Name, e.Provider)
		} else {
			fmt.Fprintf(&b, "%s%d: %s\n", marker, i, e.Name)
		}
	}
	b.WriteString("\nUse /model <n> to switch.")
	return b.String()
}

// noModelText is the reply to /model and /list models for an agent with no
// model to use: its turns fail until one of its models is in the model list
// and enabled.
func noModelText(rt *Runtime) string {
	name := "This agent"
	if rt != nil && rt.AgentName != "" {
		name = rt.AgentName
	}
	return name + " has no usable model: its models are not in the model list or disabled."
}
