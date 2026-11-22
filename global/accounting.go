// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package global

import "go.bison9.com/agent/accounting"

// Identity names who a model or API call is for: the agent it is billed to,
// a friendly label, and the project. Every Chat, Transcribe, Synthesize,
// Generate and Edit call carries one.
type Identity = accounting.Identity

// Project is the project code every call is billed to. Project codes are not
// tracked yet; this is the one place to replace when they are. It is passed
// explicitly rather than left to the SDK's empty-means-default fallback.
const Project = "default"

// NoOwnerAgent is the agent recorded for a call no agent owns: the CLI
// `test` command, and the transcription of a voice note in a message no
// agent could be resolved for. It begins with an underscore, which no agent
// id may, so it never collides with a real agent.
const NoOwnerAgent = "_host"

// NoOwnerIdentity is the identity of a call made on behalf of no agent.
var NoOwnerIdentity = Identity{Agent: NoOwnerAgent, Name: "No agent", Project: Project}
