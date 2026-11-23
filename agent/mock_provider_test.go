package agent

import (
	"context"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm"
)

type mockProvider struct{}

func (m *mockProvider) Chat(
	ctx context.Context, _ accounting.Identity,
	messages []spawnllm.Message,
	tools []spawnllm.ToolDefinition,
	model string,
	opts map[string]any,
) (*spawnllm.LLMResponse, error) {
	return &spawnllm.LLMResponse{
		Content:   "Mock response",
		ToolCalls: []spawnllm.ToolCall{},
	}, nil
}

func (m *mockProvider) GetDefaultModel() string {
	return "mock-model"
}
