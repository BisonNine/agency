// ClawEh
// License: MIT

package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/mcphost"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/tools"
)

func TestOptions_EmptyClearsEarlierValue(t *testing.T) {
	reg := newRegistryWith(&mockTool{name: "read_file", params: map[string]any{}, result: toolkit.NewResult("ok")})
	regs := map[string]*tools.ToolRegistry{"alice": reg}

	t.Run("registries", func(t *testing.T) {
		_, err := New(WithAgentRegistries(regs), WithAgentRegistries(nil))
		if err == nil || !strings.Contains(err.Error(), "agent registry") {
			t.Fatalf("New() = %v, want the missing-registry error", err)
		}
	})
	t.Run("workspaces", func(t *testing.T) {
		srv, err := New(WithAgentRegistries(regs),
			WithAgentWorkspaces(map[string]string{"alice": "/w/alice"}), WithAgentWorkspaces(nil))
		if err != nil {
			t.Fatal(err)
		}
		if srv.workspaces != nil || srv.tracker.workspace("alice") != "" {
			t.Fatalf("workspaces = %v, want none", srv.workspaces)
		}
	})
	t.Run("allowlists", func(t *testing.T) {
		srv, err := New(WithAgentRegistries(regs), WithAllowlist([]string{"*"}),
			WithInternalAllowlist(nil), WithExternalAllowlist([]string{}))
		if err != nil {
			t.Fatal(err)
		}
		if srv.internalAllow != nil || srv.externalAllow != nil {
			t.Fatalf("allowlists = %v / %v, want none", srv.internalAllow, srv.externalAllow)
		}
		_, body := rpc(context.Background(), srv.httpServer.Handler, InternalEndpointPath, "", "tools/list", map[string]any{})
		if strings.Contains(string(body), "read_file") {
			t.Fatalf("/internal lists a tool with an empty allowlist: %s", body)
		}
	})
}

func TestRegister_EmptyTokenDropsTheSessionsToken(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()
	st := newSessionTokenStore()
	old := st.Issue("alice", "sess", "/a")

	st.Register("", "alice", "sess", "/a")

	if _, ok := st.Resolve(old); ok {
		t.Fatal("the session's earlier token still resolves")
	}
	if !strings.Contains(buf.String(), "session token not registered") {
		t.Fatalf("log lacks the refusal:\n%s", buf.String())
	}
	// The session has no token now: the next Issue makes a fresh, unpinned one.
	tok := st.Issue("alice", "sess", "/a")
	if tok == "" || tok == old {
		t.Fatalf("Issue after the refused Register = %q", tok)
	}
	if rec, ok := st.Resolve(tok); !ok || rec.pinned {
		t.Fatalf("Resolve(new) = %+v, %v; want an unpinned record", rec, ok)
	}
}

func TestSetDepth_SessionWithoutTokenIsNoOp(t *testing.T) {
	st := newSessionTokenStore()
	tok := st.Issue("alice", "sess", "/a")

	st.SetDepth("", 3)
	st.SetDepth("other", 3)
	st.SetTurnScope("other", []string{"bob"}, "task")

	rec, ok := st.Resolve(tok)
	if !ok || rec.depth != 0 || rec.askChain != nil || rec.taskID != "" {
		t.Fatalf("Resolve = %+v, %v; another session's update reached it", rec, ok)
	}
	if _, ok := st.bySess["other"]; ok {
		t.Fatal("an update created a token for a session that had none")
	}
}

func TestLogSkip_SchemaAndRefused(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	logSkip(mcphost.Skip{Internal: "plain_err", Reason: mcphost.SkipSchema, Err: errors.New("bad schema")})
	logSkip(mcphost.Skip{Internal: "no_err", Reason: mcphost.SkipSchema})
	logSkip(mcphost.Skip{Internal: "refused_tool", Published: "refused_ext", Reason: mcphost.SkipRefused})

	out := buf.String()
	for _, want := range []string{
		"skipping tool: failed to marshal schema", "plain_err", "bad schema", "no_err",
		"skipping tool: MCP refused its schema", "refused_tool", "refused_ext",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// The host's callbacks fail closed on a call that carries none of the
// host's data.
func TestHostCallbacks_WithoutHostData(t *testing.T) {
	srv, err := New(minimalOpts(newRegistryWith())...)
	if err != nil {
		t.Fatal(err)
	}
	h := newEndpointHost(srv, false, nil)
	ctx := context.Background()

	if res := h.gate(ctx, toolkit.Info{Name: "read_file"}); res == nil || !res.IsError {
		t.Fatalf("gate on a call that resolved no tool = %+v, want a refusal", res)
	}
	if res, err := h.beforeTool(ctx, &hook.ToolCall{Name: "read_file"}); res != nil || err != nil {
		t.Fatalf("beforeTool without a caller = %+v, %v; want nothing", res, err)
	}
	if got := internalName(ctx, "published"); got != "published" {
		t.Fatalf("internalName without a target = %q", got)
	}
	if got := (*firstCallTracker)(nil).workspace("alice"); got != "" {
		t.Fatalf("nil tracker workspace = %q", got)
	}
}
