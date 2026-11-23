// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/toolspec"
)

func TestToolsScopeFailureIsReturned(t *testing.T) {
	st := svcToolSetup(t)
	for name := range st.defs {
		res, err := st.callAs("denied", name, map[string]any{"id": "x"})
		if err == nil || res != nil || !strings.Contains(err.Error(), "may not use the forum tools") {
			t.Errorf("%s as a denied agent = %+v, %v; want the scope error", name, res, err)
		}
	}
}

func TestToolsBaseNotUsable(t *testing.T) {
	e := svcSetup(t)
	host := &svcToolHost{base: filepath.Join(e.workspace, "doc.md"), workspace: e.workspace}
	defs := map[string]toolspec.ToolDefinition{}
	for _, d := range Tools(e.svc, host) {
		defs[d.Name] = d
	}
	call := func(name string) *toolspec.Result {
		t.Helper()
		res, err := defs[name].Handler(&toolspec.ToolCall{Ctx: t.Context(), Args: map[string]any{}, AgentID: "launcher"})
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("%s = %+v, %v; want a tool error", name, res, err)
		}
		return res
	}
	if got := call("new").ForLLM; got != "The forum could not be created because of an internal error." {
		t.Errorf("new = %q", got)
	}
	if got := call("status").ForLLM; !strings.Contains(got, "because of an internal error") {
		t.Errorf("status = %q", got)
	}
}

func TestToolMessageNamesEveryAgentLeft(t *testing.T) {
	e := svcSetup(t)
	ts := &toolSuite{svc: e.svc}
	err := &agentsLeftError{forumID: "f", ref: "Alice forum", agents: []string{"a1", "a2"}, err: errors.New("x")}
	got := ts.message(err, "Alice forum", "delete")
	if !strings.Contains(got, "temporary agents a1, a2 could not be deleted") {
		t.Errorf("message = %q", got)
	}
}

func TestSentenceOfBlank(t *testing.T) {
	if got := sentence("  "); got != "" {
		t.Errorf("sentence(blank) = %q", got)
	}
}
