// Tests for Name(), Description(), Parameters() of various tools — simple but needed for coverage.

package tools_test

import (
	"testing"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
	toolsmsg "go.bison9.com/agency/tools/msg"
	toolsshell "go.bison9.com/agency/tools/shell"
)

func TestMessageTool_Metadata(t *testing.T) {
	tool := toolsmsg.NewMessageTool()
	if tool.Name() != "msg_send" {
		t.Errorf("Name() = %q, want msg_send", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	if params == nil {
		t.Error("Parameters() should not be nil")
	}
}

func TestMessageTool_ResetSentInRound(t *testing.T) {
	tool := toolsmsg.NewMessageTool()
	tool.ResetSentInRound()
	if tool.HasSentInRound() {
		t.Error("HasSentInRound() should be false after Reset")
	}
}

func TestMessageTool_HasSentInRound_Initial(t *testing.T) {
	tool := toolsmsg.NewMessageTool()
	if tool.HasSentInRound() {
		t.Error("HasSentInRound() should be false initially")
	}
}

func TestDiscoveryTools_Metadata(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterDiscoveryTools(10, nil)
	for _, name := range []string{"search_tools", "get_tool_details"} {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Description() == "" {
			t.Errorf("%s: Description() should not be empty", name)
		}
		if tool.Parameters() == nil {
			t.Errorf("%s: Parameters() should not be nil", name)
		}
	}
}

func TestSendFileTool_Metadata(t *testing.T) {
	tool := toolsmsg.NewSendFileTool(nil, false, 0, nil)
	if tool.Name() != "msg_send_file" {
		t.Errorf("Name() = %q, want msg_send_file", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	if params == nil {
		t.Error("Parameters() should not be nil")
	}
}

func TestExecTool_Metadata(t *testing.T) {
	cfg := config.DefaultConfig()
	tool, err := toolsshell.NewExecToolWithConfig(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("NewExecToolWithConfig() error = %v", err)
	}
	if tool.Name() != "shell_exec" {
		t.Errorf("Name() = %q, want shell_exec", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	if params == nil {
		t.Error("Parameters() should not be nil")
	}
}
