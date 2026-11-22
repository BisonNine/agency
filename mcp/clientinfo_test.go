package mcp

import (
	"testing"

	"go.bison9.com/agency/app"
)

// TestClientInfo_IsTheRealProduct pins what an MCP server is told about its
// client: the product name and the release number, not a placeholder.
func TestClientInfo_IsTheRealProduct(t *testing.T) {
	got := clientInfo()
	if got.Name != app.Name() || got.Version != app.SemVer() {
		t.Errorf("clientInfo() = %+v, want name %q version %q", got, app.Name(), app.SemVer())
	}
}
