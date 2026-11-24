// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package providers

import (
	"go.bison9.com/agent/models"
)

// FallbackNotify is called when a chain moves past one or more models: the
// attempts just passed (one failure, or a run of cooldown skips) and the
// model tried next (models.WithNotify).
type FallbackNotify = func(passed []models.Attempt, next models.Candidate)
