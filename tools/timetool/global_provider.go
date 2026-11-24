// Agency
// License: MIT

package timetool

import (
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// GlobalProvider exposes the time tool through the transport-neutral global
// layer with the BARE name "now". The aggregator mounts it under the "time"
// namespace, so the published name is "time_now".
var GlobalProvider globalTimeProvider

type globalTimeProvider struct{}

func (globalTimeProvider) Namespace() string   { return "time" }
func (globalTimeProvider) Description() string { return "Current date, time, and timezone" }

func (globalTimeProvider) Available(_ any) (bool, string) { return true, "" }

func (globalTimeProvider) RegisterTools(_ toolspec.Deps) []toolspec.ToolDefinition {
	now := NewNowTool()
	return []toolspec.ToolDefinition{
		{
			Name:        now.Name(),
			Description: now.Description(),
			RawSchema:   now.Parameters(),
			Category:    "context",
			// On by default: the system prompt carries only a date, and the
			// prompt points the model here for the time. Withholding the tool
			// while telling the model to use it is the one combination that
			// leaves it unable to answer a question about the current time.
			DefaultAllow: new(true),
			Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
				return toolkit.ResultToSpec(now.Execute(call.Ctx, call.Args)), nil
			},
		},
	}
}
