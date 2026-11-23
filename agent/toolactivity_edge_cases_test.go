package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm"
)

func TestToolActivity_Summary(t *testing.T) {
	cases := []struct {
		desc    string
		tool    string
		args    map[string]any
		want    string
		notWant string
	}{
		{
			desc: "insert keyed by after_line shows @line",
			tool: "file_insert_lines",
			args: map[string]any{"path": "dir/notes.md", "after_line": 7.0, "text": "secret"},
			want: "🔧 `file_insert_lines` @7 `notes.md`",
		},
		{
			desc: "range edit without a numeric start shows the file only",
			tool: "file_delete_lines",
			args: map[string]any{"path": "dir/notes.md"},
			want: "🔧 `file_delete_lines` `notes.md`",
		},
		{
			desc: "range edit with start only",
			tool: "file_delete_lines",
			args: map[string]any{"path": "notes.md", "start": 4.0},
			want: "🔧 `file_delete_lines` 4 `notes.md`",
		},
		{
			desc: "range edit with end before start shows the start",
			tool: "file_edit_lines",
			args: map[string]any{"path": "notes.md", "start": 9.0, "end": 3.0},
			want: "🔧 `file_edit_lines` 9 `notes.md`",
		},
		{
			desc: "move shows source and destination basenames",
			tool: "file_move",
			args: map[string]any{"source_path": "a/old.md", "destination_path": "b/new.md"},
			want: "🔧 `file_move` `old.md` → `new.md`",
		},
		{
			desc: "copy with only a destination",
			tool: "mcp__files__file_copy",
			args: map[string]any{"destination_path": "b/new.md"},
			want: "🔧 `file_copy` `` → `new.md`",
		},
		{
			desc: "move without paths falls back to the bare name",
			tool: "file_move",
			args: map[string]any{},
			want: "🔧 `file_move`",
		},
		{
			desc: "read lines with non-positive start and count uses the defaults",
			tool: "file_read_lines",
			args: map[string]any{"path": "x.md", "start_line": -3.0, "line_count": 0.0},
			want: "🔧 `file_read_lines` 1–250 `x.md`",
		},
		{
			desc:    "web_fetch with a malformed URL shows no URL",
			tool:    "web_fetch",
			args:    map[string]any{"url": "http://[::1"},
			want:    "🔧 `web_fetch`",
			notWant: "::1",
		},
		{
			desc:    "web_fetch with a URL that has no host does not leak its query",
			tool:    "web_fetch",
			args:    map[string]any{"url": "example.com/search?q=secret"},
			want:    "🔧 `web_fetch`",
			notWant: "secret",
		},
		{
			desc: "web_fetch with nil args",
			tool: "web_fetch",
			args: nil,
			want: "🔧 `web_fetch`",
		},
		{
			desc: "generic detail keeps at most three values in key order",
			tool: "metar_get",
			args: map[string]any{"a": "one", "b": 2.0, "c": true, "d": "four"},
			want: "🔧 `metar_get` `one` `2` `true`",
		},
		{
			desc:    "generic detail skips secrets, long strings and non-scalars",
			tool:    "mcp__svc__lookup",
			args:    map[string]any{"api_key": "k", "blank": "  ", "long": strings.Repeat("x", 41), "list": []any{"a"}, "ratio": 1.5},
			want:    "🔧 `lookup` `1.5`",
			notWant: "xxxx",
		},
	}
	for _, c := range cases {
		got := toolActivitySummary(c.tool, c.args)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("%s: breadcrumb leaked %q: %q", c.desc, c.notWant, got)
		}
	}
}

func TestToolActivity_ToolCallBreadcrumb(t *testing.T) {
	cases := []struct {
		desc string
		tc   spawnllm.ToolCall
		want string
	}{
		{
			desc: "name and arguments from the function call",
			tc:   spawnllm.ToolCall{Function: &spawnllm.FunctionCall{Name: "file_write", Arguments: `{"path":"d/out.md","content":"body"}`}},
			want: "🔧 `file_write` `out.md`",
		},
		{
			desc: "unparseable arguments summarise by name alone",
			tc:   spawnllm.ToolCall{Function: &spawnllm.FunctionCall{Name: "file_write", Arguments: `{"path":`}},
			want: "🔧 `file_write`",
		},
		{
			desc: "blank function arguments",
			tc:   spawnllm.ToolCall{Function: &spawnllm.FunctionCall{Name: "status", Arguments: "  "}},
			want: "🔧 `status`",
		},
		{
			desc: "decoded arguments win over the function call",
			tc: spawnllm.ToolCall{
				Name:      "file_read_bytes",
				Arguments: map[string]any{"path": "a.md"},
				Function:  &spawnllm.FunctionCall{Name: "other", Arguments: `{"path":"b.md"}`},
			},
			want: "🔧 `file_read_bytes` `a.md`",
		},
		{
			desc: "no name at all",
			tc:   spawnllm.ToolCall{},
			want: "",
		},
	}
	for _, c := range cases {
		if got := toolCallBreadcrumb(c.tc); got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
	}
}

func TestToolActivity_ToolArgInt(t *testing.T) {
	cases := []struct {
		desc   string
		args   map[string]any
		want   int
		wantOK bool
	}{
		{"nil args", nil, 0, false},
		{"missing", map[string]any{}, 0, false},
		{"float64", map[string]any{"n": 12.9}, 12, true},
		{"int", map[string]any{"n": 5}, 5, true},
		{"int64", map[string]any{"n": int64(6)}, 6, true},
		{"json.Number", map[string]any{"n": json.Number("8")}, 8, true},
		{"bad json.Number", map[string]any{"n": json.Number("8.5")}, 0, false},
		{"numeric string", map[string]any{"n": " 9 "}, 9, true},
		{"non-numeric string", map[string]any{"n": "nine"}, 0, false},
		{"bool", map[string]any{"n": true}, 0, false},
	}
	for _, c := range cases {
		got, ok := toolArgInt(c.args, "n")
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.desc, got, ok, c.want, c.wantOK)
		}
	}
}

func TestToolActivity_TrimFloat(t *testing.T) {
	cases := map[float64]string{3: "3", -2: "-2", 0.25: "0.25"}
	for in, want := range cases {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v) = %q, want %q", in, got, want)
		}
	}
}
