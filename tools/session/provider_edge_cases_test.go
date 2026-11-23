package session

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	sessiontools "go.bison9.com/agent/ctxengine/tools"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/app"
	"go.bison9.com/agency/tools"
)

func sessionTool(t *testing.T, deps tools.ToolDeps, name string) toolkit.Tool {
	t.Helper()
	for _, tl := range tools.NamespacedProvider("session", GlobalProvider).Build(deps) {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("no %s", name)
	return nil
}

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "session" || GlobalProvider.Description() != "Session history and management tools" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}

// session_clear is refused while the call cannot be tied to one turn.
func TestSessionClear_RefusedWhenAmbiguous(t *testing.T) {
	cleared := 0
	clearTool := sessionTool(t, tools.ToolDeps{ClearFn: func(context.Context, string, string) error { cleared++; return nil }}, "session_clear")
	ctx := toolkit.WithSessionKey(toolkit.WithChat(context.Background(), "telegram", "1"), "agent:alice:main")
	ctx = tools.WithReplyChat(ctx, tools.ReplyChat{Channel: "telegram", ChatID: "1", Ambiguous: true})
	res := clearTool.Execute(ctx, map[string]any{})
	if !res.IsError || !toolkit.IsExpectedRefusal(res.Err) || !strings.Contains(res.ForLLM, "answering more than one conversation") || cleared != 0 {
		t.Errorf("ambiguous: %+v cleared=%d", res, cleared)
	}
}

// session_info reports the running build and platform; a failure of the
// host's lookup is passed on.
func TestSessionInfo_ServerIdentity(t *testing.T) {
	var fail error
	deps := tools.ToolDeps{
		Workspace: t.TempDir(),
		SessionInfoFn: func(_ context.Context, key string) (*sessiontools.SessionInfo, error) {
			if fail != nil {
				return nil, fail
			}
			return &sessiontools.SessionInfo{SessionKey: key}, nil
		},
	}
	info := sessionTool(t, deps, "session_info")
	ctx := toolkit.WithSessionKey(toolkit.WithChat(context.Background(), "telegram", "1"), "agent:alice:main")
	res := info.Execute(ctx, map[string]any{})
	if res.IsError || !strings.Contains(res.ForLLM, app.Name()+" "+app.Version()) || !strings.Contains(res.ForLLM, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("info: %s", res.ForLLM)
	}
	fail = errors.New("no such session")
	if res := info.Execute(ctx, map[string]any{}); !res.IsError || !strings.Contains(res.ForLLM, "no such session") {
		t.Errorf("failure: %s", res.ForLLM)
	}
}
