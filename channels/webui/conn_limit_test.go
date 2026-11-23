package webui

import (
	"net/http"
	"testing"
	"time"

	"go.bison9.com/agency/config"
)

// Handshakes in progress at the same moment count against MaxConnections: with
// every admitted handshake held inside the upgrade, only the limit's worth get
// that far and the rest are refused, so no more than the limit connect.
func TestHandleWebSocket_ConcurrentConnectsRespectLimit(t *testing.T) {
	const (
		limit = 2
		dials = 6
	)
	c, _, wsURL := liveChannel(t, config.WebUIConfig{MaxConnections: limit})

	arrived := make(chan struct{}, dials)
	release := make(chan struct{})
	checkOrigin := c.upgrader.CheckOrigin
	c.upgrader.CheckOrigin = func(r *http.Request) bool {
		arrived <- struct{}{}
		<-release
		if checkOrigin == nil {
			return true
		}
		return checkOrigin(r)
	}

	type result struct {
		ok     bool
		status int
	}
	results := make(chan result, dials)
	for range dials {
		go func() {
			conn, status, err := dialToken(t, wsURL)
			results <- result{ok: err == nil && conn != nil, status: status}
		}()
	}

	// Every dial either reaches the upgrade (and waits there) or is refused.
	var inUpgrade, refused, connected int
	deadline := time.After(10 * time.Second)
	for inUpgrade+refused < dials {
		select {
		case <-arrived:
			inUpgrade++
		case r := <-results:
			if r.ok {
				t.Fatal("a connection completed while the upgrade was held")
			}
			if r.status != http.StatusServiceUnavailable {
				t.Fatalf("refused with status %d, want 503", r.status)
			}
			refused++
		case <-deadline:
			t.Fatalf("waiting for dials: %d in upgrade, %d refused", inUpgrade, refused)
		}
	}
	close(release)
	for range inUpgrade {
		select {
		case r := <-results:
			if r.ok {
				connected++
			}
		case <-deadline:
			t.Fatal("held handshakes did not complete")
		}
	}

	if inUpgrade != limit || refused != dials-limit || connected != limit {
		t.Fatalf("in upgrade %d, refused %d, connected %d; want %d, %d, %d",
			inUpgrade, refused, connected, limit, dials-limit, limit)
	}
	if n := c.connCount.Load(); n != limit {
		t.Fatalf("connCount = %d, want %d", n, limit)
	}
}
