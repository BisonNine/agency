package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	acplib "github.com/a3tai/openclaw-go/acp"
	"github.com/a3tai/openclaw-go/protocol"
)

// acpInput passes every line but session/cancel through in order, hands each
// session/cancel to onCancel (absent or unreadable parameters as the zero
// value), and ends with the input.
func TestACPInput_SplitsCancels(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"acp-a"}}`,
		`not json`,
		`{"jsonrpc":"2.0","method":"session/cancel"}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":"odd"}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}`,
	}, "\n")
	var cancels []string
	r := acpInput(strings.NewReader(input), func(n acplib.CancelNotification) {
		cancels = append(cancels, n.SessionID)
	})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}` + "\n" +
		"not json\n" +
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}` + "\n"
	if string(out) != want {
		t.Fatalf("forwarded %q, want %q", out, want)
	}
	if !slices.Equal(cancels, []string{"acp-a", "", ""}) {
		t.Fatalf("cancels = %q", cancels)
	}
}

// A session/cancel is handed over while the reader is not being read (the
// ACP server busy with a prompt), and the lines queued meanwhile are read
// afterwards.
func TestACPInput_CancelWhileNotRead(t *testing.T) {
	in, w := io.Pipe()
	got := make(chan string, 1)
	r := acpInput(in, func(n acplib.CancelNotification) { got <- n.SessionID })
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(w, `{"id":3,"method":"session/set_mode"}`+"\n"+`{"method":"session/cancel","params":{"sessionId":"acp-b"}}`+"\n")
		written <- errors.Join(err, w.Close())
	}()
	select {
	case sid := <-got:
		if sid != "acp-b" {
			t.Fatalf("cancel for %q", sid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session/cancel not handed over while the reader was idle")
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil || line != `{"id":3,"method":"session/set_mode"}`+"\n" {
		t.Fatalf("queued line = %q (err %v)", line, err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write: %v", err)
	}
}

// A line longer than the ACP server accepts ends the input with the error.
func TestACPInput_LineTooLong(t *testing.T) {
	r := acpInput(strings.NewReader(strings.Repeat("x", maxACPLine+1)+"\n"), func(acplib.CancelNotification) {})
	if _, err := io.ReadAll(r); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want %v", err, bufio.ErrTooLong)
	}
}

// Small reads get a line in pieces.
func TestACPInput_SmallReads(t *testing.T) {
	r := acpInput(strings.NewReader("abcdef\n"), func(acplib.CancelNotification) {})
	buf := make([]byte, 4)
	var out []byte
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(out) != "abcdef\n" {
		t.Fatalf("read %q", out)
	}
}

// endingGateway delivers the run's own end event while chat.abort is in
// flight, as the gateway may.
type endingGateway struct {
	br *acpBridge
}

func (g *endingGateway) ChatSend(context.Context, protocol.ChatSendParams) (*protocol.ChatEvent, error) {
	return nil, errors.New("unused")
}

func (g *endingGateway) ChatAbort(_ context.Context, p protocol.ChatAbortParams) error {
	raw, err := json.Marshal(map[string]any{"runId": p.RunID, "stream": "lifecycle", "data": map[string]any{"phase": "end"}})
	if err != nil {
		return err
	}
	g.br.handleGatewayEvent(protocol.Event{EventName: protocol.EventAgent, Payload: raw})
	return nil
}

// A cancelled prompt stays cancelled when the run's end event arrives before
// chat.abort returns.
func TestACPBridgeCancel_EndEventDuringAbort(t *testing.T) {
	g := &endingGateway{}
	br := newACPBridge(g)
	g.br = br
	run := &bridgeRun{sessionID: "acp-a", done: make(chan struct{})}
	br.runs["run-a"] = run
	br.Cancel(context.Background(), acplib.CancelNotification{SessionID: "acp-a"})
	if run.stop != acplib.StopReasonCancelled {
		t.Fatalf("stop = %q, want cancelled", run.stop)
	}
}
