package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/discovery"
)

// Dummy tool to fill the registry in our tests.
type mockSearchableTool struct {
	name string
	desc string
}

func (m *mockSearchableTool) Name() string        { return m.name }
func (m *mockSearchableTool) Description() string { return m.desc }
func (m *mockSearchableTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}

func (m *mockSearchableTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return SilentResult("mock executed: " + m.name)
}

// Helper to initialize a populated ToolRegistry
func setupPopulatedRegistry() *ToolRegistry {
	reg := NewToolRegistry()

	// A core tool (NOT to be found by searches)
	reg.Register(&mockSearchableTool{
		name: "core_search",
		desc: "I am a visible core tool for searching files",
	})

	// Hidden tools (must be found by searches)
	registerHidden(reg, &mockSearchableTool{
		name: "mcp_read_file",
		desc: "Read the contents of a system file",
	})
	registerHidden(reg, &mockSearchableTool{
		name: "mcp_list_dir",
		desc: "List directories and files in the system",
	})
	registerHidden(reg, &mockSearchableTool{
		name: "mcp_fetch_net",
		desc: "Fetch data from a network database",
	})

	return reg
}

func TestSearchTool_RegexMode(t *testing.T) {
	reg := setupPopulatedRegistry()
	tool, _ := discoveryTools(reg, 10)
	ctx := context.Background()

	t.Run("Empty Query Error", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"regex": true})
		if !res.IsError || !strings.Contains(res.ForLLM, "Missing or invalid 'query'") {
			t.Errorf("Expected missing query error, got: %v", res.ForLLM)
		}
	})

	t.Run("Invalid Regex Syntax", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "[unclosed", "regex": true})
		if !res.IsError || !strings.Contains(res.ForLLM, "Invalid regex pattern syntax") {
			t.Errorf("Expected regex syntax error, got: %v", res.ForLLM)
		}
	})

	t.Run("No Match Found", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "alien", "regex": true})
		if res.IsError || !strings.Contains(res.ForLLM, "No tools found matching") {
			t.Errorf("Expected 'no tools found' message, got: %v", res.ForLLM)
		}
	})

	t.Run("Match names and references the hits", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "system", "regex": true})
		if res.IsError {
			t.Fatalf("Unexpected error: %v", res.ForLLM)
		}
		if !strings.Contains(res.ForLLM, "get_tool_details") {
			t.Errorf("Expected search to point at get_tool_details, got: %v", res.ForLLM)
		}
		want := []string{"mcp_list_dir", "mcp_read_file"}
		if !slices.Equal(res.ToolReferences, want) {
			t.Errorf("ToolReferences = %v, want %v", res.ToolReferences, want)
		}
		for _, name := range want {
			if !strings.Contains(res.ForLLM, "- "+name+": ") {
				t.Errorf("result text lacks a line for %s: %q", name, res.ForLLM)
			}
		}
	})
}

func TestSearchTool_BM25Mode(t *testing.T) {
	reg := setupPopulatedRegistry()
	tool, _ := discoveryTools(reg, 3)
	ctx := context.Background()

	t.Run("Empty Query Error", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "   "})
		if !res.IsError || !strings.Contains(res.ForLLM, "Missing or invalid 'query'") {
			t.Errorf("Expected missing query error, got: %v", res.ForLLM)
		}
	})

	t.Run("No Match Found", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "aliens spaceships"})
		if res.IsError || !strings.Contains(res.ForLLM, "No tools found matching") {
			t.Errorf("Expected 'no tools found', got: %v", res.ForLLM)
		}
	})

	t.Run("Match names and references the hits", func(t *testing.T) {
		res := tool.Execute(ctx, map[string]any{"query": "read files"})
		if res.IsError {
			t.Fatalf("Unexpected error: %v", res.ForLLM)
		}
		if !strings.Contains(res.ForLLM, "- mcp_read_file: Read the contents of a system file") {
			t.Errorf("Expected a line for mcp_read_file in BM25 results, got: %q", res.ForLLM)
		}
		if !slices.Contains(res.ToolReferences, "mcp_read_file") {
			t.Errorf("ToolReferences = %v, want mcp_read_file among them", res.ToolReferences)
		}
		for _, ref := range res.ToolReferences {
			if !strings.Contains(res.ForLLM, "- "+ref+": ") {
				t.Errorf("reference %s has no line in the text %q", ref, res.ForLLM)
			}
		}
	})
}

func TestToolDetailsTool_Details(t *testing.T) {
	reg := setupPopulatedRegistry()
	_, details := discoveryTools(reg, 10)
	ctx := context.Background()

	t.Run("Unknown tool errors", func(t *testing.T) {
		res := details.Execute(ctx, map[string]any{"name": "no_such_tool"})
		if !res.IsError || !strings.Contains(res.ForLLM, "No tool named") {
			t.Errorf("Expected unknown-tool error, got: %v", res.ForLLM)
		}
	})

	t.Run("Schema text and one reference", func(t *testing.T) {
		res := details.Execute(ctx, map[string]any{"name": "mcp_read_file"})
		if res.IsError {
			t.Fatalf("Unexpected error: %v", res.ForLLM)
		}
		if !strings.Contains(res.ForLLM, `"name":"mcp_read_file"`) || !strings.Contains(res.ForLLM, "Read the contents of a system file") {
			t.Errorf("Expected the tool's schema in the text, got: %v", res.ForLLM)
		}
		if want := []string{"mcp_read_file"}; !slices.Equal(res.ToolReferences, want) {
			t.Errorf("ToolReferences = %v, want %v", res.ToolReferences, want)
		}
	})

	t.Run("Core tool: schema, no reference", func(t *testing.T) {
		res := details.Execute(ctx, map[string]any{"name": "core_search"})
		if res.IsError {
			t.Fatalf("Unexpected error: %v", res.ForLLM)
		}
		if len(res.ToolReferences) != 0 {
			t.Errorf("ToolReferences = %v, want none for a core tool", res.ToolReferences)
		}
	})
}

func TestSearchTool_PatternTooLong(t *testing.T) {
	reg := setupPopulatedRegistry()
	tool, _ := discoveryTools(reg, 10)
	ctx := context.Background()

	longPattern := strings.Repeat("a", discovery.MaxRegexPatternLength+1)
	res := tool.Execute(ctx, map[string]any{"query": longPattern, "regex": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "Pattern too long") {
		t.Errorf("Expected pattern too long error, got: %v", res.ForLLM)
	}
}

// search_tools returns at most the configured number of hits, and never a
// listed tool.
func TestSearchTool_LimitsAndCoreFiltering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		max   int
		regex bool
	}{
		{"regex", 4, true},
		{"bm25", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewToolRegistry()
			reg.Register(&mockSearchableTool{"core_match", "I am core with match"})
			for i := range 10 {
				registerHidden(reg, &mockSearchableTool{
					name: fmt.Sprintf("hidden_match_%d", i),
					desc: "this has a match",
				})
			}
			search, _ := discoveryTools(reg, tc.max)
			res := search.Execute(context.Background(), map[string]any{"query": "match", "regex": tc.regex})
			if len(res.ToolReferences) != tc.max {
				t.Errorf("%d hits, want exactly %d", len(res.ToolReferences), tc.max)
			}
			if slices.Contains(res.ToolReferences, "core_match") {
				t.Error("search returned a listed tool")
			}
		})
	}
}

func TestBM25CacheInvalidation(t *testing.T) {
	reg := NewToolRegistry()
	registerHidden(reg, &mockSearchableTool{name: "tool_alpha", desc: "alpha functionality"})

	tool, _ := discoveryTools(reg, 10)
	ctx := context.Background()

	// First search should find tool_alpha
	res := tool.Execute(ctx, map[string]any{"query": "alpha"})
	if !strings.Contains(res.ForLLM, "tool_alpha") {
		t.Fatalf("Expected 'tool_alpha' in first search, got: %v", res.ForLLM)
	}

	// Register a new hidden tool
	registerHidden(reg, &mockSearchableTool{name: "tool_beta", desc: "beta functionality"})

	// Cache should be invalidated; new tool should be findable
	res = tool.Execute(ctx, map[string]any{"query": "beta"})
	if !strings.Contains(res.ForLLM, "tool_beta") {
		t.Errorf("Expected 'tool_beta' after cache invalidation, got: %v", res.ForLLM)
	}
}
