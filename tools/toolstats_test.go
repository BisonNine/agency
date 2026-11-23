// ClawEh
// License: MIT

package tools

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.bison9.com/agent/toolkit"
)

// The tally itself is toolkit's (and tested there); the host's part is the
// redaction it is built with.
func TestToolStats_LastErrorRedactedAndTruncated(t *testing.T) {
	const key = "agent:clone-redact:main"
	token := "SST" + strings.Repeat("a", 64)
	BeginToolStats(key)
	RecordToolResult(key, "cogmem_status", toolkit.ErrorResult("rejected "+token+" "+strings.Repeat("x", 1000)))
	_, _, last := EndToolStats(key)
	if strings.Contains(last, token) {
		t.Errorf("token not redacted: %q", last)
	}
	if !strings.Contains(last, "[REDACTED]") {
		t.Errorf("redaction marker missing: %q", last)
	}
	text := strings.TrimPrefix(last, "cogmem_status: ")
	if n := utf8.RuneCountInString(text); n != toolkit.MaxLastErrorRunes+1 {
		t.Errorf("error text is %d runes, want %d plus the ellipsis", n, toolkit.MaxLastErrorRunes)
	}
}
