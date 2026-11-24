// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/global"
)

// recordCapture collects the accounting records written while installed.
type recordCapture struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (c *recordCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *recordCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *recordCapture) WithGroup(string) slog.Handler            { return c }
func (c *recordCapture) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(at slog.Attr) bool {
		var rec map[string]any
		if at.Key == "accounting" && json.Unmarshal([]byte(at.Value.String()), &rec) == nil {
			c.mu.Lock()
			c.recs = append(c.recs, rec)
			c.mu.Unlock()
		}
		return true
	})
	return nil
}

// The test command's calls belong to no agent and are billed to the
// default code, named.
func TestModelsBilledToNoOwnerDefault(t *testing.T) {
	recs := &recordCapture{}
	accounting.SetLogger(slog.New(recs))
	t.Cleanup(func() { accounting.SetLogger(nil) })
	stubConfig(t, alice)
	_, err := runTest(t, "models")
	require.NoError(t, err)
	recs.mu.Lock()
	defer recs.mu.Unlock()
	require.NotEmpty(t, recs.recs)
	for _, r := range recs.recs {
		require.Equal(t, global.NoOwnerAgent, r["agent"])
		require.Equal(t, global.DefaultBilling, r["project"])
	}
}
