// Agency
// License: MIT

package api

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.bison9.com/agency/msgtoken"
)

// failingCreateLoop fails every token mint.
type failingCreateLoop struct{ *fakeMessageTokenLoop }

func (failingCreateLoop) CreateMessageToken(string, string) (msgtoken.NamedToken, error) {
	return msgtoken.NamedToken{}, errors.New("store full")
}

// Create and update refuse a body that is not JSON; update refuses negative
// limits; a mint that fails is a 500 without detail; the name is trimmed.
func TestMessageTokens_CreateAndUpdateRefusals(t *testing.T) {
	h, mux, loop := newMessageTokenTestHandler(t)
	base := "/api/agents/main/message-tokens"
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{http.MethodPost, base, "{", http.StatusBadRequest, `{"error":"invalid body"}`},
		{http.MethodPatch, base + "/id1", "{", http.StatusBadRequest, `{"error":"invalid body"}`},
		{http.MethodPatch, base + "/id1", `{"rate_per_min":-1}`, http.StatusBadRequest, `{"error":"rate_per_min and block_minutes must be \u003e= 0"}`},
		{http.MethodPatch, base + "/id1", `{"block_minutes":-1}`, http.StatusBadRequest, `{"error":"rate_per_min and block_minutes must be \u003e= 0"}`},
	} {
		rec := serve(mux, tc.method, tc.path, tc.body)
		if rec.Code != tc.status || rec.Body.String() != tc.want+"\n" {
			t.Errorf("%s %s %s = %d %q", tc.method, tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}

	rec := serve(mux, http.MethodPost, base, `{"name":"  Alice's app  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	if got := loop.ListMessageTokens("main"); len(got) != 1 || got[0].Name != "Alice's app" {
		t.Fatalf("stored = %+v, want one token named Alice's app", got)
	}

	h.SetMessageTokenLoop(failingCreateLoop{loop})
	rec = serve(mux, http.MethodPost, base, `{"name":"x"}`)
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"create failed"}`+"\n" {
		t.Fatalf("failing mint = %d %q", rec.Code, rec.Body.String())
	}
}

// Before the gateway wires the agent loop, listing is empty, minting is a
// 503 and the rest are 404s.
func TestMessageTokens_NoLoop(t *testing.T) {
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	base := "/api/agents/main/message-tokens"
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{http.MethodGet, base, "", http.StatusOK, `{"endpoint_base":"http://example.com/api/message/","tokens":[]}`},
		{http.MethodPost, base, `{"name":"x"}`, http.StatusServiceUnavailable, `{"error":"message tokens unavailable"}`},
		{http.MethodPatch, base + "/t", `{}`, http.StatusNotFound, `{"error":"token not found"}`},
		{http.MethodDelete, base + "/t", "", http.StatusNotFound, `{"error":"token not found"}`},
	} {
		rec := serve(mux, tc.method, tc.path, tc.body)
		if rec.Code != tc.status || rec.Body.String() != tc.want+"\n" {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.path, rec.Code, rec.Body.String(), tc.status, tc.want)
		}
	}
}

// The endpoint base is https when the request came over TLS or a proxy says
// it did.
func TestMessageEndpointBase(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://agency.example:18790/x", nil)
	if got := messageEndpointBase(r); got != "http://agency.example:18790/api/message/" {
		t.Fatalf("plain = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS")
	if got := messageEndpointBase(r); got != "https://agency.example:18790/api/message/" {
		t.Fatalf("forwarded = %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "http://agency.example:18443/x", nil)
	r.TLS = &tls.ConnectionState{}
	if got := messageEndpointBase(r); got != "https://agency.example:18443/api/message/" {
		t.Fatalf("tls = %q", got)
	}
}
