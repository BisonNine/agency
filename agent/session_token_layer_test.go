// Agency
// License: MIT

package agent

import "testing"

// The text a CLI model receives for its MCP session token, captured before
// the layer was built by the SDK's mcphost.TokenPrompt. It must not change:
// models are told to pass the token exactly as this text says.
func TestSessionTokenLayer_TextUnchanged(t *testing.T) {
	const want = "# Session Token\n\nThe following token is confidential — never echo it to users or write it to files. " +
		"ALL `mcp__agency__*` tool calls MUST include the literal string below as the `session_token` parameter.\n\n" +
		"session_token: SST0123"
	l := sessionTokenLayer("SST0123", "")
	if l.Text != want {
		t.Errorf("session token layer changed:\n got %q\nwant %q", l.Text, want)
	}
	if l.Name != "session_token" || !l.AfterSummary {
		t.Errorf("layer = %+v, want session_token after the summary", l)
	}
}
