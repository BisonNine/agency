package channels

import (
	"slices"
	"strings"
	"testing"
)

// SplitMessage around fenced code blocks, at 100 runes per message.
func TestSplitMessage_CodeBlocks(t *testing.T) {
	x70, c30 := strings.Repeat("x", 70), strings.Repeat("c", 30)
	fenceCont := func(body string) string { return "```\n" + body + "\n```" }
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			// The block closes within 100 runes of the start: the first
			// message extends to its closing fence.
			name:    "extends to the closing fence",
			content: "```go\n" + x70 + "\n```\n" + strings.Repeat("y", 30),
			want:    []string{"```go\n" + x70 + "\n```", strings.Repeat("y", 30)},
		},
		{
			// No newline anywhere: the header is the bare fence, and the
			// block is cut at the limit and reopened.
			name:    "unclosed block without a newline",
			content: "```" + strings.Repeat("x", 120),
			want:    []string{"```" + strings.Repeat("x", 92) + "\n```", "```\n" + strings.Repeat("x", 28)},
		},
		{
			// Cut at the last newline inside the block; the continuation
			// reopens it with no blank line added.
			name:    "unclosed block cut at a newline",
			content: "```\n" + strings.Repeat(c30+"\n", 4),
			want:    []string{fenceCont(c30 + "\n" + c30), "```\n" + c30 + "\n" + c30},
		},
		{
			// The block opens near the cut and cannot fit: the text before
			// it goes first, split at its last newline.
			name:    "text before the block, split at a newline",
			content: "hello world\n" + strings.Repeat("a", 25) + "\n```\n" + strings.Repeat("c", 120),
			want: []string{
				"hello world\n" + strings.Repeat("a", 25),
				"```\n" + strings.Repeat("c", 91) + "\n```",
				"```\n" + strings.Repeat("c", 29),
			},
		},
		{
			// Nowhere to split before the block, which starts more than 20
			// runes in: the text before it goes first, whole.
			name:    "text before the block, no split point",
			content: strings.Repeat("a", 30) + "```\n" + strings.Repeat("c", 120),
			want: []string{
				strings.Repeat("a", 30),
				"```\n" + strings.Repeat("c", 91) + "\n```",
				"```\n" + strings.Repeat("c", 29),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitMessage(tt.content, 100)
			for i, chunk := range got {
				if n := len([]rune(chunk)); n > 100 {
					t.Errorf("chunk %d is %d runes, over the limit", i, n)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("SplitMessage =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestSplitMessage_NoLimitEmpty(t *testing.T) {
	if got := SplitMessage("", 0); got != nil {
		t.Fatalf("SplitMessage(\"\", 0) = %q, want nil", got)
	}
}
