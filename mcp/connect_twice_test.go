// ClawEh
// License: MIT

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"go.bison9.com/agency/config"
)

// A second connect for a server that is already connected (a reload's Sync
// racing the retry loop) does not replace the live connection: the new
// client is closed, so neither connection is leaked, and callers keep using
// the one they already hold.
func TestConnectServer_DoesNotReplaceLiveConnection(t *testing.T) {
	srv := server.NewMCPServer("test", "0.0.1")
	srv.AddTool(mcp.NewTool("ping", mcp.WithDescription("no-op")), noopHandler)
	// open counts the requests the server is still serving: each live
	// client holds one, its tools-changed notification stream.
	var open atomic.Int32
	mcpHandler := server.NewStreamableHTTPServer(srv)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open.Add(1)
		defer open.Add(-1)
		mcpHandler.ServeHTTP(w, r)
	}))
	defer killServer(t, ts)

	mgr := NewManager()
	defer func() {
		if err := mgr.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: ts.URL}
	ctx := context.Background()
	if err := mgr.ConnectServer(ctx, "svc", cfg); err != nil {
		t.Fatal(err)
	}
	first, _ := mgr.GetServer("svc")
	streams := waitOpen(t, &open, func(n int32) bool { return n > 0 })

	if err := mgr.ConnectServer(ctx, "svc", cfg); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if got, _ := mgr.GetServer("svc"); got != first {
		t.Error("the live connection was replaced")
	}
	// The redundant client's stream ends; only the kept one's remains.
	waitOpen(t, &open, func(n int32) bool { return n == streams })
	if _, err := mgr.CallTool(ctx, "svc", "ping", nil); err != nil {
		t.Errorf("the kept connection does not work: %v", err)
	}
}

// waitOpen waits until ok(open) holds and returns the count then.
func waitOpen(t *testing.T, open *atomic.Int32, ok func(int32) bool) int32 {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if n := open.Load(); ok(n) {
			return n
		}
		select {
		case <-deadline:
			t.Fatalf("open server requests = %d", open.Load())
		case <-time.After(time.Millisecond):
		}
	}
}
