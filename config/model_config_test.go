// Agency
// License: MIT

package config

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/modelspec"
)

func TestGetModelConfig_Found(t *testing.T) {
	cfg := &Config{
		Models: []modelspec.Model{
			{ModelName: "test-model", Model: "openai/gpt-4o", Enabled: true},
			{ModelName: "other-model", Model: "anthropic/claude", Enabled: true},
		},
	}

	result, err := cfg.GetModelConfig("test-model")
	if err != nil {
		t.Fatalf("GetModelConfig() error = %v", err)
	}
	if result.Model != "openai/gpt-4o" {
		t.Errorf("Model = %q, want %q", result.Model, "openai/gpt-4o")
	}
}

func TestGetModelConfig_NotFound(t *testing.T) {
	cfg := &Config{
		Models: []modelspec.Model{
			{ModelName: "test-model", Model: "openai/gpt-4o"},
		},
	}

	_, err := cfg.GetModelConfig("nonexistent")
	if err == nil {
		t.Fatal("GetModelConfig() expected error for nonexistent model")
	}
}

func TestGetModelConfig_EmptyList(t *testing.T) {
	cfg := &Config{
		Models: []modelspec.Model{},
	}

	_, err := cfg.GetModelConfig("any-model")
	if err == nil {
		t.Fatal("GetModelConfig() expected error for empty model list")
	}
}

func TestGetModelConfig_RoundRobin(t *testing.T) {
	cfg := &Config{
		Models: []modelspec.Model{
			{ModelName: "lb-model", Model: "openai/gpt-4o-1", Enabled: true},
			{ModelName: "lb-model", Model: "openai/gpt-4o-2", Enabled: true},
			{ModelName: "lb-model", Model: "openai/gpt-4o-3", Enabled: true},
		},
	}

	// Test round-robin distribution
	results := make(map[string]int)
	for range 30 {
		result, err := cfg.GetModelConfig("lb-model")
		if err != nil {
			t.Fatalf("GetModelConfig() error = %v", err)
		}
		results[result.Model]++
	}

	// Each model should appear roughly 10 times (30 calls / 3 models)
	for model, count := range results {
		if count < 5 || count > 15 {
			t.Errorf("Model %s appeared %d times, expected ~10", model, count)
		}
	}
}

func TestGetModelConfig_Concurrent(t *testing.T) {
	cfg := &Config{
		Models: []modelspec.Model{
			{ModelName: "concurrent-model", Model: "openai/gpt-4o-1", Enabled: true},
			{ModelName: "concurrent-model", Model: "openai/gpt-4o-2", Enabled: true},
		},
	}

	const goroutines = 100
	const iterations = 10

	var wg sync.WaitGroup
	errors := make(chan error, goroutines*iterations)

	for range goroutines {
		wg.Go(func() {
			for range iterations {
				_, err := cfg.GetModelConfig("concurrent-model")
				if err != nil {
					errors <- err
				}
			}
		})
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("Concurrent GetModelConfig() error: %v", err)
	}
}

func TestAgentDefaults_DefaultModelName(t *testing.T) {
	tests := []struct {
		name     string
		defaults AgentDefaults
		wantName string
	}{
		{
			name:     "nil model returns empty",
			defaults: AgentDefaults{},
			wantName: "",
		},
		{
			name:     "model with primary set",
			defaults: AgentDefaults{Models: []string{"gpt4"}},
			wantName: "gpt4",
		},
		{
			name:     "model with primary and fallbacks",
			defaults: AgentDefaults{Models: []string{"gpt4", "claude"}},
			wantName: "gpt4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.defaults.DefaultModelName(); got != tt.wantName {
				t.Errorf("DefaultModelName() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestAgentDefaults_SetDefaultModel(t *testing.T) {
	t.Run("set model on empty model list", func(t *testing.T) {
		var defaults AgentDefaults
		defaults.SetDefaultModel("gpt4")
		if len(defaults.Models) != 1 {
			t.Fatalf("Models len = %d, want 1", len(defaults.Models))
		}
		if defaults.Models[0] != "gpt4" {
			t.Errorf("Models[0] = %q, want %q", defaults.Models[0], "gpt4")
		}
	})

	t.Run("set model preserves remaining entries", func(t *testing.T) {
		defaults := AgentDefaults{
			Models: []string{"old-model", "fallback1"},
		}
		defaults.SetDefaultModel("new-model")
		if defaults.Models[0] != "new-model" {
			t.Errorf("Models[0] = %q, want %q", defaults.Models[0], "new-model")
		}
		if len(defaults.Models) != 2 || defaults.Models[1] != "fallback1" {
			t.Errorf("Models = %v, want [new-model fallback1]", defaults.Models)
		}
	})
}

func TestFullConfig_JSON_ModelConfig(t *testing.T) {
	// Test complete config with the agent model list
	jsonStr := `{
		"agents": {
			"defaults": {
				"workspace": "~/.claw/workspace",
				"models": ["gpt4", "claude"],
				"max_tokens": 4096
			}
		},
		"models": [
			{
				"model_name": "gpt4",
				"model": "openai/gpt-4o",
				"api_key": "test-key",
				"enabled": true
			}
		]
	}`

	cfg := &Config{}
	if err := json.Unmarshal([]byte(jsonStr), cfg); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	// Check that DefaultModelName returns correct value
	if got := cfg.Agents.Defaults.DefaultModelName(); got != "gpt4" {
		t.Errorf("DefaultModelName() = %q, want %q", got, "gpt4")
	}

	// Check that GetModelConfig works
	modelCfg, err := cfg.GetModelConfig("gpt4")
	if err != nil {
		t.Fatalf("GetModelConfig error: %v", err)
	}
	if modelCfg.Model != "openai/gpt-4o" {
		t.Errorf("Model = %q, want %q", modelCfg.Model, "openai/gpt-4o")
	}
}

func TestConfig_ValidateModelList(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string // partial error message to check
	}{
		{
			name: "valid list",
			config: &Config{
				Providers: []modelspec.Provider{
					{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1"},
					{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com"},
				},
				Models: []modelspec.Model{
					{ModelName: "test1", Model: "gpt-4o", Provider: "openai"},
					{ModelName: "test2", Model: "claude", Provider: "anthropic"},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid entry",
			config: &Config{
				Providers: []modelspec.Provider{
					{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1"},
				},
				Models: []modelspec.Model{
					{ModelName: "test1", Model: "gpt-4o", Provider: "openai"},
					{ModelName: "", Model: "claude", Provider: "openai"}, // missing model_name
				},
			},
			wantErr: true,
			errMsg:  "model_name is required",
		},
		{
			name: "unknown provider reference",
			config: &Config{
				Models: []modelspec.Model{
					{ModelName: "test1", Model: "gpt-4o", Provider: "nope"},
				},
			},
			wantErr: true,
			errMsg:  "provider \"nope\" not found",
		},
		{
			name: "empty list",
			config: &Config{
				Models: []modelspec.Model{},
			},
			wantErr: false,
		},
		{
			// Load balancing: multiple entries with same model_name are allowed
			name: "duplicate model_name for load balancing",
			config: &Config{
				Providers: []modelspec.Provider{
					{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1"},
				},
				Models: []modelspec.Model{
					{ModelName: "gpt-4", Model: "gpt-4o", Provider: "openai"},
					{ModelName: "gpt-4", Model: "gpt-4-turbo", Provider: "openai"},
				},
			},
			wantErr: false, // Changed: duplicates are allowed for load balancing
		},
		{
			// Load balancing: non-adjacent entries with same model_name are also allowed
			name: "duplicate model_name non-adjacent for load balancing",
			config: &Config{
				Providers: []modelspec.Provider{
					{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1"},
					{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com"},
				},
				Models: []modelspec.Model{
					{ModelName: "model-a", Model: "gpt-4o", Provider: "openai"},
					{ModelName: "model-b", Model: "claude", Provider: "anthropic"},
					{ModelName: "model-a", Model: "gpt-4-turbo", Provider: "openai"},
				},
			},
			wantErr: false, // Changed: duplicates are allowed for load balancing
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.ValidateModels()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateModels() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errMsg != "" {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("ValidateModels() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}
