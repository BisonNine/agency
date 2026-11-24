// Agency
// License: MIT

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tenebris-tech/secmsg/schema"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/utils"
)

// fakeLinker is a SecMsgLinker that returns canned replies.
type fakeLinker struct {
	reply *schema.LinkReply
	err   error
	calls []string
}

func (f *fakeLinker) RequestLink(context.Context) (*schema.LinkReply, error) {
	f.calls = append(f.calls, "request")
	return f.reply, f.err
}

func (f *fakeLinker) LinkState(context.Context) (*schema.LinkReply, error) {
	f.calls = append(f.calls, "state")
	return f.reply, f.err
}

func secmsgMux(t *testing.T) (*Handler, *http.ServeMux) {
	t.Helper()
	h := NewHandler(setupTestEnv(t))
	mux := http.NewServeMux()
	h.registerSecMsgRoutes(mux)
	return h, mux
}

// Before the gateway wires the channel lookup the link endpoints answer 503;
// an unknown channel is a 404.
func TestSecMsgLink_NotReadyAndUnknown(t *testing.T) {
	h, mux := secmsgMux(t)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec := serve(mux, method, "/api/channels/secmsg/signal/link", "")
		if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"error":"channel manager not ready"}`+"\n" {
			t.Fatalf("%s not ready: %d %q", method, rec.Code, rec.Body.String())
		}
	}
	h.SetSecMsgLinker(func(string) (SecMsgLinker, bool) { return nil, false })
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec := serve(mux, method, "/api/channels/secmsg/signal/link", "")
		if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"secmsg channel not found: signal"}`+"\n" {
			t.Fatalf("%s unknown: %d %q", method, rec.Code, rec.Body.String())
		}
	}
}

// POST starts linking and GET polls it, each on the named channel; a pairing
// URI comes back with its QR rendered, and a daemon failure is a 502 with
// the daemon's error.
func TestSecMsgLink_RequestAndStatus(t *testing.T) {
	h, mux := secmsgMux(t)
	linker := &fakeLinker{reply: &schema.LinkReply{Status: "pending", URI: "sgnl://linkdevice?uuid=abc"}}
	var asked []string
	h.SetSecMsgLinker(func(name string) (SecMsgLinker, bool) {
		asked = append(asked, name)
		return linker, true
	})

	rec := serve(mux, http.MethodPost, "/api/channels/secmsg/signal/link", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("request: %d %s", rec.Code, rec.Body.String())
	}
	var got secmsgLinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || got.URI != "sgnl://linkdevice?uuid=abc" || !strings.HasPrefix(got.QRPng, "data:image/png;base64,") {
		t.Fatalf("request reply = %+v", got)
	}

	linker.reply = &schema.LinkReply{Status: "linked", Phone: "+15550100"}
	rec = serve(mux, http.MethodGet, "/api/channels/secmsg/signal/link", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"linked","phone":"+15550100"}`+"\n" {
		t.Fatalf("status: %d %q", rec.Code, rec.Body.String())
	}
	if strings.Join(linker.calls, ",") != "request,state" || strings.Join(asked, ",") != "signal,signal" {
		t.Fatalf("calls = %v, asked = %v", linker.calls, asked)
	}

	linker.err = errors.New("daemon unreachable")
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec = serve(mux, method, "/api/channels/secmsg/signal/link", "")
		if rec.Code != http.StatusBadGateway || rec.Body.String() != `{"error":"daemon unreachable"}`+"\n" {
			t.Fatalf("%s failure: %d %q", method, rec.Code, rec.Body.String())
		}
	}
}

// startStatusDaemon runs a minimal secmsg daemon on a kernel-picked loopback
// port that greets each client and answers the status RPC with accounts.
func startStatusDaemon(t *testing.T, accounts []schema.StatusReply) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer utils.CloseQuietly(c)
				enc := json.NewEncoder(c)
				hello := map[string]any{
					"jsonrpc": "2.0", "method": schema.MethodHello,
					"params": schema.InfoParams{Proto: schema.ProtoVersion, Daemon: "fakesigd", Service: "signal"},
				}
				if enc.Encode(hello) != nil {
					return
				}
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     uint64 `json:"id"`
						Method string `json:"method"`
					}
					if json.Unmarshal(line, &req) != nil {
						continue
					}
					resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
					if req.Method == schema.MethodStatus {
						resp["result"] = schema.StatusAllReply{Accounts: accounts}
					} else {
						resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
					}
					if enc.Encode(resp) != nil {
						return
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		utils.CloseQuietly(ln)
		wg.Wait()
	})
	return ln.Addr().String()
}

// GET /api/channels/secmsg/{name}/accounts lists the daemon's linked
// accounts only; an unknown daemon is a 404 and an unreachable one a 502.
func TestSecMsgAccounts(t *testing.T) {
	h, mux := secmsgMux(t)
	addr := startStatusDaemon(t, []schema.StatusReply{
		{Account: "+15550100", Linked: true},
		{Account: "+15550101", Linked: false},
	})
	// A listener that is closed at once gives an address nothing answers on.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	utils.CloseQuietly(dead)

	if err := h.updateConfig(nil, func(c *config.Config) error {
		c.Channels.SecMsg = []config.SecMsgConfig{
			{Name: "", Address: addr},
			{Name: "down", Address: deadAddr},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rec := serve(mux, http.MethodGet, "/api/channels/secmsg/secmsg/accounts", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"accounts":["+15550100"]}`+"\n" {
		t.Fatalf("accounts: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodGet, "/api/channels/secmsg/nope/accounts", "")
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"secmsg daemon not found: nope"}`+"\n" {
		t.Fatalf("unknown: %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(mux, http.MethodGet, "/api/channels/secmsg/down/accounts", "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"error":"dial:`) {
		t.Fatalf("unreachable: %d %q", rec.Code, rec.Body.String())
	}
}
