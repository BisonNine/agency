package secmsg

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tenebris-tech/secmsg/schema"

	"go.bison9.com/agency/utils"
)

// fakeRPCError is a JSON-RPC error object the fake daemon returns.
type fakeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fakeHandler answers one RPC method: a result to marshal, or an error.
type fakeHandler func(params json.RawMessage) (any, *fakeRPCError)

// fakeRequest is one RPC the fake daemon received.
type fakeRequest struct {
	conn   *fakeConn
	method string
	params json.RawMessage
	// answered is closed once the reply has been written to the client.
	answered chan struct{}
}

// fakeConn is one accepted client connection on the fake daemon.
type fakeConn struct {
	conn net.Conn
	wmu  sync.Mutex
}

// write sends one JSON line to the client.
func (fc *fakeConn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fc.wmu.Lock()
	defer fc.wmu.Unlock()
	_, err = fc.conn.Write(append(data, '\n'))
	return err
}

// notify pushes a server notification, as the daemon does for inbound messages.
func (fc *fakeConn) notify(t *testing.T, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	if err := fc.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": json.RawMessage(raw)}); err != nil {
		t.Fatalf("push notification: %v", err)
	}
}

// fakeDaemon is an in-process stand-in for a secmsg daemon (e.g. sigd)
// listening on a kernel-picked 127.0.0.1 port. It greets each connection with
// a hello advertising service, answers requests from handlers, and records
// every request so tests can assert what the channel sent.
type fakeDaemon struct {
	t       *testing.T
	ln      net.Listener
	service string

	mu       sync.Mutex
	handlers map[string]fakeHandler

	reqs chan fakeRequest
}

func newFakeDaemon(t *testing.T, service string) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDaemon{
		t:       t,
		ln:      ln,
		service: service,
		reqs:    make(chan fakeRequest, 256),
		handlers: map[string]fakeHandler{
			schema.MethodSubscribe: func(json.RawMessage) (any, *fakeRPCError) {
				return map[string]any{"subscribed": true}, nil
			},
			schema.MethodSend:      func(json.RawMessage) (any, *fakeRPCError) { return "ok", nil },
			schema.MethodSendGroup: func(json.RawMessage) (any, *fakeRPCError) { return "ok", nil },
		},
	}
	var wg sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, c)
			connsMu.Unlock()
			wg.Go(func() { d.serve(c) })
		}
	})
	t.Cleanup(func() {
		utils.CloseQuietly(ln)
		connsMu.Lock()
		for _, c := range conns {
			utils.CloseQuietly(c)
		}
		connsMu.Unlock()
		wg.Wait()
	})
	return d
}

// addr is the daemon's dial address.
func (d *fakeDaemon) addr() string { return d.ln.Addr().String() }

// handle installs (or replaces) the handler for method.
func (d *fakeDaemon) handle(method string, h fakeHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[method] = h
}

// linkedAccounts makes the status RPC report the given linked accounts plus
// one unlinked account that must never be selected.
func (d *fakeDaemon) linkedAccounts(ids ...string) {
	d.handle(schema.MethodStatus, func(json.RawMessage) (any, *fakeRPCError) {
		reply := schema.StatusAllReply{Accounts: []schema.StatusReply{{Account: "unlinked", Linked: false}}}
		for _, id := range ids {
			reply.Accounts = append(reply.Accounts, schema.StatusReply{Account: id, Linked: true})
		}
		return reply, nil
	})
}

func (d *fakeDaemon) serve(c net.Conn) {
	fc := &fakeConn{conn: c}
	hello := map[string]any{
		"jsonrpc": "2.0",
		"method":  schema.MethodHello,
		"params":  schema.InfoParams{Proto: schema.ProtoVersion, Daemon: "fakesigd", Service: d.service},
	}
	if err := fc.write(hello); err != nil {
		return
	}
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     uint64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		answered := make(chan struct{})
		d.reqs <- fakeRequest{conn: fc, method: req.Method, params: req.Params, answered: answered}

		d.mu.Lock()
		h := d.handlers[req.Method]
		d.mu.Unlock()
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if h == nil {
			resp["error"] = fakeRPCError{Code: -32601, Message: "method not found: " + req.Method}
		} else if result, rerr := h(req.Params); rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		err = fc.write(resp)
		close(answered)
		if err != nil {
			return
		}
	}
}

// expect waits for the next request and fails unless it is for method.
func (d *fakeDaemon) expect(method string) fakeRequest {
	d.t.Helper()
	select {
	case req := <-d.reqs:
		if req.method != method {
			d.t.Fatalf("daemon got %q (params %s), want %q", req.method, req.params, method)
		}
		return req
	case <-time.After(5 * time.Second):
		d.t.Fatalf("daemon did not receive %q", method)
		return fakeRequest{}
	}
}

// waitAnswered blocks until the daemon has written its reply to req.
func (d *fakeDaemon) waitAnswered(req fakeRequest) {
	d.t.Helper()
	select {
	case <-req.answered:
	case <-time.After(5 * time.Second):
		d.t.Fatalf("daemon did not answer %q", req.method)
	}
}

// closedAddr returns an address on which nothing listens, so dialling it fails
// at once with connection refused.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
