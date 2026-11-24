// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package global

import "go.bison9.com/agent/accounting"

// Identity names who a model or API call is for: the agent it is billed to,
// a friendly label, and the billing code (the SDK's Project). Every Chat,
// Transcribe, Synthesize, Generate and Edit call carries one, and every one
// the host builds names its billing code deliberately.
type Identity = accounting.Identity

// DefaultBilling is the billing code of work nobody chose one for: a session
// that never set one, a scheduled job, classifier or task record written
// before billing codes existed, and the operator's own commands.
const DefaultBilling = accounting.DefaultProject

// MemoryBilling is the billing code of memory consolidation, which runs for
// an agent over the messages of several sessions and so belongs to none of
// their codes.
const MemoryBilling = "memory"

// NoOwnerAgent is the agent recorded for a call no agent owns: the CLI
// `test` command, and the transcription of a voice note in a message no
// agent could be resolved for. It begins with an underscore, which no agent
// id may, so it never collides with a real agent.
const NoOwnerAgent = "_host"

// NoOwnerIdentity is the identity of a call made on behalf of no agent,
// billed to billing.
func NoOwnerIdentity(billing string) Identity {
	return Identity{Agent: NoOwnerAgent, Name: "No agent", Project: billing}
}
