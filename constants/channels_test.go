package constants_test

import (
	"testing"

	"go.bison9.com/agency/constants"
)

func TestIsInternalChannel(t *testing.T) {
	tests := []struct {
		channel string
		want    bool
	}{
		{"cli", true},
		{"system", true},
		{"subagent", true},
		{"recovery", true},
		{constants.AgentMessageChannel, true},
		{constants.SubagentChannel, true},
		{"telegram", false},
		{"slack", false},
		{"", false},
		{"CLI", false},
	}
	for _, tt := range tests {
		if got := constants.IsInternalChannel(tt.channel); got != tt.want {
			t.Errorf("IsInternalChannel(%q) = %v, want %v", tt.channel, got, tt.want)
		}
	}
}

func TestRepliesToAgent(t *testing.T) {
	tests := []struct {
		channel string
		want    bool
	}{
		{constants.AgentMessageChannel, true},
		{constants.SubagentChannel, true},
		{"cli", false},
		{"system", false},
		{"recovery", false},
		{"telegram", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := constants.RepliesToAgent(tt.channel); got != tt.want {
			t.Errorf("RepliesToAgent(%q) = %v, want %v", tt.channel, got, tt.want)
		}
	}
}
