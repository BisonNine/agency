// ClawEh
// License: MIT

package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/health"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestMessageRoute_Outcomes: a bad token is 401, an unreadable body 400, a
// delivered message 202, and a bus that refuses it 500.
func TestMessageRoute_Outcomes(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			BaseDir:  t.TempDir(),
			Defaults: config.AgentDefaults{Models: []string{"test-model"}, MaxTokens: 4096, MaxToolIterations: 10},
			List:     []config.AgentConfig{{ID: "main", Name: "Main", Default: true}},
		},
		Bindings: []config.AgentBinding{{
			AgentID: "main", Default: true,
			Match: config.BindingMatch{Channel: "telegram-main", Peer: &config.PeerMatch{Kind: "direct", ID: "4242"}},
		}},
	}
	msgBus := bus.NewMessageBus()
	al, err := agent.NewAgentLoop(t.Context(), cfg, msgBus)
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	tok, err := al.CreateMessageToken("main", "gps")
	if err != nil {
		t.Fatal(err)
	}
	server := health.NewServer("127.0.0.1", 0)
	mux := http.NewServeMux()
	server.RegisterOnMux(mux)
	RegisterMessageRoute(server, al)

	post := func(token string, body io.Reader) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/message/"+token, body))
		return rec.Code
	}
	if code := post("not-a-token", strings.NewReader("hello")); code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", code)
	}
	if code := post(tok.Token, errReader{}); code != http.StatusBadRequest {
		t.Errorf("unreadable body = %d, want 400", code)
	}
	if code := post(tok.Token, strings.NewReader("position update")); code != http.StatusAccepted {
		t.Errorf("valid message = %d, want 202", code)
	}
	msgBus.Close()
	if code := post(tok.Token, strings.NewReader("after close")); code != http.StatusInternalServerError {
		t.Errorf("closed bus = %d, want 500", code)
	}
}
