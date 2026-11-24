// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package billing is the billing tool: one tool, published under the bare
// name "billing", with which an agent reads or sets the billing code its
// model calls are recorded under. Every agent has it (config.BillingSuite);
// deny_tools can remove it.
package billing

import (
	"context"
	"errors"
	"fmt"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/tools"
)

// Suite is the suite's name and the tool's.
const Suite = config.BillingSuite

// Texts the tool answers with.
const (
	currentText = "Billing code: "
	setText     = "Billing code set to %s."
	invalidText = "Invalid billing code: use 1 to 64 letters, digits, '-', '_' or '.'."
	failedText  = "The billing code could not be saved."
	unavailText = "Billing codes are not available here."
)

const description = "Show or set the billing code your model calls are recorded under. " +
	"Without code, it returns the current billing code. With code, it sets it: the new code applies from your next model call " +
	"and stays until changed, also for work you start afterwards (sub-agents, scheduled jobs, agents you ask). " +
	"Work that was started for you (a sub-agent, a scheduled job) changes only its own code."

// GlobalProvider mounts the billing tool. Register it with
// tools.BareNamespacedProvider so the tool is published as "billing".
var GlobalProvider globalBillingProvider

type globalBillingProvider struct{}

func (globalBillingProvider) Namespace() string { return Suite }
func (globalBillingProvider) Description() string {
	return "Billing code: read or set the code model calls are recorded under"
}
func (globalBillingProvider) Available(any) (bool, string) { return true, "" }

// Suite marks the tool as a suite: no tools-list entry grants it, every
// configured agent has it (config.Config.AgentSuiteEnabled).
func (globalBillingProvider) Suite() string { return Suite }

// RegisterTools returns the billing tool, bound to the agent loop's billing
// closures; nothing in the deps-free enumeration pass.
func (globalBillingProvider) RegisterTools(deps toolspec.Deps) []toolspec.ToolDefinition {
	host, ok := deps.Host.(tools.ToolDeps)
	if !ok || host.Cfg == nil {
		return nil
	}
	return []toolspec.ToolDefinition{definition(host.BillingOf, host.SetBilling)}
}

// definition is the billing tool over get (the current code of the work
// calling on a context) and set (nil when the code cannot be changed here).
func definition(get func(context.Context) string, set func(ctx context.Context, sessionKey, code string) error) toolspec.ToolDefinition {
	return toolspec.ToolDefinition{
		Name:        Suite,
		Description: description,
		Parameters: []toolspec.Parameter{{
			Name:        "code",
			Type:        "string",
			Description: "The new billing code: 1 to 64 letters, digits, '-', '_' or '.'. Leave it out to see the current code.",
		}},
		SessionScoped: true,
		Category:      Suite,
		Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
			raw, given := call.Args["code"]
			if !given || raw == nil {
				return &toolspec.Result{ForLLM: currentText + get(call.Ctx)}, nil
			}
			code, isString := raw.(string)
			if !isString {
				return &toolspec.Result{ForLLM: invalidText, IsError: true}, nil
			}
			if err := accounting.CheckProject(code); err != nil {
				return &toolspec.Result{ForLLM: invalidText, IsError: true, Err: err}, nil //nolint:nilerr // an invalid code is the model's error, answered as a tool result
			}
			if set == nil {
				return &toolspec.Result{ForLLM: unavailText, IsError: true}, nil
			}
			if err := set(call.Ctx, call.Session, code); err != nil {
				if errors.Is(err, accounting.ErrInvalidProject) {
					return &toolspec.Result{ForLLM: invalidText, IsError: true, Err: err}, nil
				}
				logger.WarnCF("billing", "Billing code not set",
					map[string]any{"agent_id": call.AgentID, "session_key": call.Session, "error": err.Error()})
				return &toolspec.Result{ForLLM: failedText, IsError: true, Err: err}, nil
			}
			return &toolspec.Result{ForLLM: fmt.Sprintf(setText, code)}, nil
		},
	}
}
