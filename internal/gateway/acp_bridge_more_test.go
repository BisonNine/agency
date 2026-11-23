package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	acplib "github.com/a3tai/openclaw-go/acp"
	"github.com/a3tai/openclaw-go/protocol"
)

// failingGateway refuses chat.send and chat.abort.
type failingGateway struct {
	mu     sync.Mutex
	aborts []string
}

func (f *failingGateway) ChatSend(context.Context, protocol.ChatSendParams) (*protocol.ChatEvent, error) {
	return nil, errors.New("gateway gone")
}

func (f *failingGateway) ChatAbort(_ context.Context, p protocol.ChatAbortParams) error {
	f.mu.Lock()
	f.aborts = append(f.aborts, p.RunID)
	f.mu.Unlock()
	return errors.New("abort refused")
}

// failingNotifier refuses every session update.
type failingNotifier struct{ calls int }

func (f *failingNotifier) SessionUpdate(acplib.SessionNotification) error {
	f.calls++
	return errors.New("client gone")
}

// TestACPBridgeSessionManagement: the session probes answer benignly and the
// sessions they name are remembered.
func TestACPBridgeSessionManagement(t *testing.T) {
	ctx := context.Background()
	br := newACPBridge(&fakeGateway{})
	if _, err := br.Authenticate(ctx, acplib.AuthenticateRequest{}); err != nil {
		t.Errorf("Authenticate: %v", err)
	}
	if _, err := br.LoadSession(ctx, acplib.LoadSessionRequest{SessionID: "acp-load"}); err != nil {
		t.Errorf("LoadSession: %v", err)
	}
	if _, err := br.ResumeSession(ctx, acplib.ResumeSessionRequest{SessionID: "acp-resume"}); err != nil {
		t.Errorf("ResumeSession: %v", err)
	}
	if _, err := br.ResumeSession(ctx, acplib.ResumeSessionRequest{}); err != nil {
		t.Errorf("ResumeSession without an id: %v", err)
	}
	fork, err := br.ForkSession(ctx, acplib.ForkSessionRequest{})
	if err != nil || !strings.HasPrefix(fork.SessionID, "acp-") {
		t.Errorf("ForkSession = %+v, %v", fork, err)
	}
	list, err := br.ListSessions(ctx, acplib.ListSessionsRequest{})
	if err != nil || list.Sessions == nil || len(list.Sessions) != 0 {
		t.Errorf("ListSessions = %+v, %v; want an empty, non-nil list", list, err)
	}
	if _, err := br.SetSessionMode(ctx, acplib.SetSessionModeRequest{}); err != nil {
		t.Errorf("SetSessionMode: %v", err)
	}
	if _, err := br.SetSessionModel(ctx, acplib.SetSessionModelRequest{}); err != nil {
		t.Errorf("SetSessionModel: %v", err)
	}
	opts, err := br.SetSessionConfigOption(ctx, acplib.SetSessionConfigOptionRequest{})
	if err != nil || opts.ConfigOptions == nil {
		t.Errorf("SetSessionConfigOption = %+v, %v", opts, err)
	}
	br.mu.Lock()
	defer br.mu.Unlock()
	for _, id := range []string{"acp-load", "acp-resume", fork.SessionID} {
		if _, ok := br.sessions[id]; !ok {
			t.Errorf("session %s not remembered", id)
		}
	}
	if _, ok := br.sessions[""]; ok {
		t.Error("an empty session id was remembered")
	}
}

// TestACPBridgeCancel: cancelling a session aborts its runs on the gateway and
// ends their prompts as cancelled; other sessions' runs are left alone, and a
// refused abort still ends the prompt.
func TestACPBridgeCancel(t *testing.T) {
	fg := &failingGateway{}
	br := newACPBridge(fg)
	mine := &bridgeRun{sessionID: "acp-a", done: make(chan struct{})}
	other := &bridgeRun{sessionID: "acp-b", done: make(chan struct{})}
	br.runs["run-a"], br.runs["run-b"] = mine, other

	br.Cancel(context.Background(), acplib.CancelNotification{SessionID: "acp-a"})

	if !slices.Equal(fg.aborts, []string{"run-a"}) {
		t.Errorf("aborted %v, want only run-a", fg.aborts)
	}
	select {
	case <-mine.done:
		if mine.stop != acplib.StopReasonCancelled {
			t.Errorf("stop = %q, want cancelled", mine.stop)
		}
	default:
		t.Error("the cancelled run did not finish")
	}
	select {
	case <-other.done:
		t.Error("another session's run finished")
	default:
	}
	// Finishing twice keeps the first reason.
	mine.finish(acplib.StopReasonEndTurn)
	if mine.stop != acplib.StopReasonCancelled {
		t.Errorf("stop changed to %q", mine.stop)
	}
}

func TestACPBridgePromptErrors(t *testing.T) {
	br := newACPBridge(&failingGateway{})
	if _, err := br.Prompt(context.Background(), acplib.PromptRequest{}); err == nil || !strings.Contains(err.Error(), "missing sessionId") {
		t.Errorf("prompt without a session = %v", err)
	}
	_, err := br.Prompt(context.Background(), acplib.PromptRequest{SessionID: "s", Prompt: []acplib.ContentBlock{{Type: "text", Text: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "chat.send: gateway gone") {
		t.Errorf("prompt with a failing gateway = %v", err)
	}
	if len(br.runs) != 0 {
		t.Errorf("failed prompt left run %v", br.runs)
	}
}

// TestACPBridgePromptContextCancelled: a prompt whose context ends returns
// cancelled with the context's error.
func TestACPBridgePromptContextCancelled(t *testing.T) {
	fg := &fakeGateway{}
	br := newACPBridge(fg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resp *acplib.PromptResponse
	var err error
	go func() {
		defer close(done)
		resp, err = br.Prompt(ctx, acplib.PromptRequest{SessionID: "s", Prompt: []acplib.ContentBlock{{Type: "text", Text: "hi"}}})
	}()
	fg.lastRunID(t)
	cancel()
	<-done
	if !errors.Is(err, context.Canceled) || resp.StopReason != acplib.StopReasonCancelled {
		t.Fatalf("Prompt = %+v, %v; want cancelled", resp, err)
	}
}

// TestACPBridgeEvents covers the event translation edge cases: garbled
// payloads, a chat error, an empty delta falling back to text, a failing
// client and the ignored keepalive and unknown events.
func TestACPBridgeEvents(t *testing.T) {
	br := newACPBridge(&fakeGateway{})
	note := &failingNotifier{}
	br.setNotifier(note)
	run := &bridgeRun{sessionID: "s", done: make(chan struct{})}
	br.runs["r1"] = run

	br.handleGatewayEvent(protocol.Event{EventName: protocol.EventAgent, Payload: json.RawMessage(`{not json`)})
	br.handleGatewayEvent(protocol.Event{EventName: protocol.EventChat, Payload: json.RawMessage(`{not json`)})
	br.handleGatewayEvent(protocol.Event{EventName: protocol.EventTick})
	br.handleGatewayEvent(protocol.Event{EventName: "something.else"})
	br.handleGatewayEvent(chatEvent(t, "unknown-run", "final"))
	// Text without a delta is sent; the client refusing it is only logged.
	payload, _ := json.Marshal(map[string]any{"runId": "r1", "stream": "assistant", "data": map[string]any{"text": "whole"}})
	br.handleGatewayEvent(protocol.Event{EventName: protocol.EventAgent, Payload: payload})
	if note.calls != 1 {
		t.Errorf("session updates = %d, want 1", note.calls)
	}
	// A lifecycle event other than end does not finish the run.
	br.handleGatewayEvent(agentEvent(t, "r1", "lifecycle", "", "start"))
	br.handleGatewayEvent(chatEvent(t, "r1", "delta"))
	select {
	case <-run.done:
		t.Fatal("run finished early")
	default:
	}
	br.handleGatewayEvent(chatEvent(t, "r1", "error"))
	<-run.done
	if run.stop != acplib.StopReasonRefusal {
		t.Errorf("stop = %q, want refusal", run.stop)
	}
}

func TestSummarizeBlocks(t *testing.T) {
	got := summarizeBlocks([]acplib.ContentBlock{
		{Type: "text", Text: "abc"},
		{Type: "image", MimeType: "image/png", Data: "xx"},
		{Type: "audio", MimeType: "audio/wav", Data: "yyy"},
		{Type: ""},
		{Type: "resource"},
	})
	want := []string{"text(3c)", "image(image/png,2b)", "audio(audio/wav,3b)", "empty", "resource"}
	if !slices.Equal(got, want) {
		t.Errorf("summarizeBlocks = %v, want %v", got, want)
	}
}
