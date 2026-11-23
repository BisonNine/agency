package msg

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// echoTool stands in for the shared msg_send instance.
type echoTool struct{ args map[string]any }

func (e *echoTool) Name() string               { return "msg_send" }
func (e *echoTool) Description() string        { return "" }
func (e *echoTool) Parameters() map[string]any { return nil }
func (e *echoTool) Execute(_ context.Context, args map[string]any) *toolkit.Result {
	e.args = args
	return toolkit.SilentResult("sent")
}

func handlersOf(deps toolspec.Deps) map[string]toolspec.ToolHandler {
	out := map[string]toolspec.ToolHandler{}
	for _, d := range GlobalProvider.RegisterTools(deps) {
		out[d.Name] = d.Handler
	}
	return out
}

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "msg" || GlobalProvider.Description() != "Message sending to users and channels" {
		t.Errorf("metadata: %q %q", GlobalProvider.Namespace(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}

func TestGlobalProvider_NotAvailable(t *testing.T) {
	h := handlersOf(toolspec.Deps{})
	for name, want := range map[string]string{"send": "message tool not available", "send_file": "send_file tool not available"} {
		res, err := h[name](&toolspec.ToolCall{Ctx: context.Background(), Args: map[string]any{}})
		if err != nil || !res.IsError || res.ForLLM != want {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
}

// With real dependencies, msg_send runs the shared instance and
// msg_send_file reads through the agent's file sandbox, only for an agent
// granted a file-reading tool.
func TestGlobalProvider_Delegates(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "files", "a.md"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Agents.Defaults.RestrictToWorkspace = true
	send := &echoTool{}
	sender := &fakeSender{}
	ctx := chatCtx()
	args := filesArg(map[string]any{"path": "files/a.md"})

	for _, tc := range []struct {
		agent   *config.AgentConfig
		allowed bool
	}{
		{&config.AgentConfig{ID: "alice", Tools: []string{"file_read_lines"}}, true},
		{&config.AgentConfig{ID: "bob"}, false},
	} {
		h := handlersOf(toolspec.Deps{Cfg: cfg, Host: tools.ToolDeps{Cfg: cfg, AgentCfg: tc.agent, Workspace: ws, MessageTool: send, Files: sender}})
		res, err := h["send"](&toolspec.ToolCall{Ctx: ctx, Args: map[string]any{"content": "hi"}})
		if err != nil || res.IsError || res.ForLLM != "sent" || send.args["content"] != "hi" {
			t.Errorf("%s send: %v %+v", tc.agent.ID, err, res)
		}
		sender.files = nil
		res, err = h["send_file"](&toolspec.ToolCall{Ctx: ctx, Args: args})
		if err != nil || res.IsError != !tc.allowed || (len(sender.files) == 1) != tc.allowed {
			t.Errorf("%s send_file: %v %+v, sent %d", tc.agent.ID, err, res, len(sender.files))
		}
	}
}
