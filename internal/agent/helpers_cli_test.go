// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ergochat/readline"

	"go.bison9.com/agency/agent"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/test/stubprovider"
	"go.bison9.com/agency/logger"
)

func TestMain(m *testing.M) {
	// The loop logs every dispatch at INFO; keep the test output readable.
	logger.SetLevel(logger.WARN)
	os.Exit(m.Run())
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Error(cerr)
		}
		done <- buf.String()
	}()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// withStdin runs fn with os.Stdin reading input.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if _, cerr := io.WriteString(w, input); cerr != nil {
			t.Error(cerr)
		}
		if cerr := w.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	os.Stdin = r
	defer func() {
		os.Stdin = old
		if cerr := r.Close(); cerr != nil {
			t.Error(cerr)
		}
	}()
	fn()
}

// newHome writes a config for one agent "main" on the stub provider (model
// alias "m") under a fresh CLAW_HOME and returns the stub.
func newHome(t *testing.T) *stubprovider.Server {
	t.Helper()
	stub := stubprovider.New(t)
	home := t.TempDir()
	t.Setenv(global.EnvVarHome, home)
	// The readline history file lives in os.TempDir(); keep it in the test.
	t.Setenv("TMPDIR", t.TempDir())
	cfg := map[string]any{
		"providers": []map[string]any{{"name": "stub", "protocol": "openai-chat", "base_url": stub.URL(), "api_key": "k"}},
		"models": []map[string]any{
			{"model_name": "m", "model": "stub-model", "provider": "stub", "enabled": true},
			{"model_name": "other", "model": "stub-other", "provider": "stub", "enabled": true},
		},
		"agents": map[string]any{
			"base_dir": filepath.Join(home, "agents"),
			"defaults": map[string]any{"models": []string{"m"}, "request_timeout": 5, "progress_interval": -1},
			"list":     []map[string]any{{"id": "main", "default": true}},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return stub
}

// newLoop builds an agent loop from the CLAW_HOME config, for the
// interactive-mode tests.
func newLoop(t *testing.T) *agent.AgentLoop {
	t.Helper()
	cfg, err := config.LoadConfig(filepath.Join(os.Getenv(global.EnvVarHome), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	msgBus := bus.NewMessageBus()
	t.Cleanup(msgBus.Close)
	loop, err := agent.NewAgentLoop(context.Background(), cfg, msgBus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loop.Close(context.Background()) })
	return loop
}

// TestAgentCmd_SingleMessage: -m sends one message to the default agent and
// prints the model's reply.
func TestAgentCmd_SingleMessage(t *testing.T) {
	stub := newHome(t)
	stub.Script(stubprovider.Reply("hello from the stub"))
	var err error
	out := captureStdout(t, func() { err = agentCmd("hi there", "", false) })
	if err != nil {
		t.Fatalf("agentCmd: %v", err)
	}
	if !strings.Contains(out, "hello from the stub") {
		t.Errorf("output lacks the reply: %q", out)
	}
	reqs := stub.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Text(), "hi there") {
		t.Fatalf("stub saw %d requests, want one carrying the message", len(reqs))
	}
	if reqs[0].Model != "stub-model" {
		t.Errorf("model = %q, want the default stub-model", reqs[0].Model)
	}
}

// TestAgentCmd_ModelOverride: --model replaces the default model, and --debug
// turns on debug logging.
func TestAgentCmd_ModelOverride(t *testing.T) {
	stub := newHome(t)
	stub.Script(stubprovider.Reply("ok"))
	prev := logger.GetLevel()
	t.Cleanup(func() { logger.SetLevel(prev) })
	var err error
	captureStdout(t, func() { err = agentCmd("hi", "other", true) })
	if err != nil {
		t.Fatalf("agentCmd: %v", err)
	}
	if logger.GetLevel() != logger.DEBUG {
		t.Error("--debug did not set the debug log level")
	}
	reqs := stub.Requests()
	if len(reqs) != 1 || reqs[0].Model != "stub-other" {
		t.Fatalf("requests = %+v, want one to stub-other", reqs)
	}
}

func TestAgentCmd_Errors(t *testing.T) {
	t.Run("unreadable config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv(global.EnvVarHome, home)
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := agentCmd("hi", "", false)
		if err == nil || !strings.Contains(err.Error(), "error loading config") {
			t.Fatalf("err = %v, want a config load error", err)
		}
	})
	t.Run("agent workspace blocked", func(t *testing.T) {
		newHome(t)
		home := os.Getenv(global.EnvVarHome)
		if err := os.WriteFile(filepath.Join(home, "agents"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := agentCmd("hi", "", false)
		if err == nil || !strings.Contains(err.Error(), "error creating agent loop") {
			t.Fatalf("err = %v, want an agent loop error", err)
		}
	})
	t.Run("model refuses the request", func(t *testing.T) {
		stub := newHome(t)
		stub.SetDefault(stubprovider.Unauthorized())
		var err error
		captureStdout(t, func() { err = agentCmd("hi", "", false) })
		if err == nil || !strings.Contains(err.Error(), "error processing message") {
			t.Fatalf("err = %v, want a processing error", err)
		}
	})
	t.Run("unknown model", func(t *testing.T) {
		newHome(t)
		err := agentCmd("hi", "no-such-model", false)
		if err == nil || !strings.Contains(err.Error(), "error creating provider") {
			t.Fatalf("err = %v, want a provider error", err)
		}
	})
}

// TestAgentCmd_Interactive: with no -m the command reads lines from stdin until
// exit, answering each.
func TestAgentCmd_Interactive(t *testing.T) {
	stub := newHome(t)
	stub.Script(stubprovider.Reply("first answer"))
	var err error
	var out string
	withStdin(t, "question one\nexit\n", func() {
		out = captureStdout(t, func() { err = agentCmd("", "", false) })
	})
	if err != nil {
		t.Fatalf("agentCmd: %v", err)
	}
	for _, want := range []string{"Interactive mode", "first answer", "Goodbye!"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if stub.Count() != 1 {
		t.Errorf("stub saw %d requests, want 1", stub.Count())
	}
}

// TestInteractiveMode: blank lines are skipped, each other line is answered,
// and the session ends on "quit" or at end of input.
func TestInteractiveMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"quit", "\n  \nfirst\nquit\nnot sent\n"},
		{"eof", "\nfirst\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newHome(t)
			stub.Script(stubprovider.Reply("answer"))
			loop := newLoop(t)
			var out string
			withStdin(t, tc.input, func() {
				out = captureStdout(t, func() { interactiveMode(loop) })
			})
			if !strings.Contains(out, "answer") || !strings.Contains(out, "Goodbye!") {
				t.Errorf("output = %q, want the answer and Goodbye!", out)
			}
			if stub.Count() != 1 {
				t.Errorf("stub saw %d requests, want 1", stub.Count())
			}
		})
	}
}

// TestSimpleInteractiveMode: the fallback reader behaves like the readline
// one: blank lines skipped, "exit"/"quit" or end of input ends the session.
func TestSimpleInteractiveMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		sent  int
	}{
		{"exit", "\nfirst\nexit\nnot sent\n", 1},
		{"quit", "first\nsecond\nquit\n", 2},
		{"eof", "first\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newHome(t)
			stub.SetDefault(stubprovider.Reply("answer"))
			loop := newLoop(t)
			var out string
			withStdin(t, tc.input, func() {
				out = captureStdout(t, func() { simpleInteractiveMode(loop) })
			})
			if !strings.Contains(out, "You: ") || !strings.Contains(out, "answer") || !strings.Contains(out, "Goodbye!") {
				t.Errorf("output = %q, want the prompt, the answer and Goodbye!", out)
			}
			if stub.Count() != tc.sent {
				t.Errorf("stub saw %d requests, want %d", stub.Count(), tc.sent)
			}
		})
	}
}

// TestInteractive_TurnErrorContinues: a failed turn is printed and the
// session carries on with the next line, in both input modes.
func TestInteractive_TurnErrorContinues(t *testing.T) {
	for name, run := range map[string]func(*agent.AgentLoop){
		"readline": interactiveMode,
		"simple":   simpleInteractiveMode,
	} {
		t.Run(name, func(t *testing.T) {
			stub := newHome(t)
			stub.SetDefault(stubprovider.Unauthorized())
			loop := newLoop(t)
			var out string
			withStdin(t, "first\nsecond\nexit\n", func() {
				out = captureStdout(t, func() { run(loop) })
			})
			if strings.Count(out, "Error: ") != 2 || !strings.Contains(out, "Goodbye!") {
				t.Errorf("output = %q, want two errors then Goodbye!", out)
			}
		})
	}
}

// TestSimpleInteractiveMode_ReadErrorStops: a read error other than end of
// input ends the session instead of retrying the failed read forever.
func TestSimpleInteractiveMode_ReadErrorStops(t *testing.T) {
	newHome(t)
	loop := newLoop(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if cerr := w.Close(); cerr != nil {
		t.Error(cerr)
	}
	if cerr := r.Close(); cerr != nil { // reads now fail with os.ErrClosed, not io.EOF
		t.Error(cerr)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	done := make(chan struct{})
	go func() {
		defer close(done)
		captureStdout(t, func() { simpleInteractiveMode(loop) })
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("simpleInteractiveMode kept retrying a failed read")
	}
}

// TestNewAgentCommand_RunsMessage: the command's flags reach agentCmd.
func TestNewAgentCommand_RunsMessage(t *testing.T) {
	stub := newHome(t)
	stub.Script(stubprovider.Reply("via cobra"))
	cmd := NewAgentCommand()
	cmd.SetArgs([]string{"-m", "hello", "--model", "other"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "via cobra") {
		t.Errorf("output = %q", out)
	}
	if reqs := stub.Requests(); len(reqs) != 1 || reqs[0].Model != "stub-other" {
		t.Errorf("requests = %+v, want one to stub-other", reqs)
	}
}

// fakeLines is a line editor that returns lines, then err for every later
// read, counting the reads.
type fakeLines struct {
	lines  []string
	err    error
	reads  int
	closed bool
}

func (f *fakeLines) Readline() (string, error) {
	f.reads++
	if len(f.lines) > 0 {
		l := f.lines[0]
		f.lines = f.lines[1:]
		return l, nil
	}
	return "", f.err
}

func (f *fakeLines) Close() error { f.closed = true; return nil }

func stubLineReader(t *testing.T, fn func(*readline.Config) (lineReader, error)) {
	t.Helper()
	orig := newLineReader
	t.Cleanup(func() { newLineReader = orig })
	newLineReader = fn
}

// TestInteractiveMode_ReadErrorStops: a line editor error other than
// end of input or an interrupt is logged once and ends the session instead of
// being retried forever.
func TestInteractiveMode_ReadErrorStops(t *testing.T) {
	stub := newHome(t)
	stub.SetDefault(stubprovider.Reply("answer"))
	loop := newLoop(t)
	fake := &fakeLines{lines: []string{"first"}, err: errors.New("terminal gone")}
	stubLineReader(t, func(*readline.Config) (lineReader, error) { return fake, nil })
	done := make(chan string)
	go func() { done <- captureStdout(t, func() { interactiveMode(loop) }) }()
	select {
	case out := <-done:
		if !strings.Contains(out, "answer") {
			t.Errorf("output = %q", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("interactiveMode kept retrying a failed read")
	}
	if fake.reads != 2 || !fake.closed {
		t.Errorf("reads = %d, closed = %v; want 2 reads and the editor closed", fake.reads, fake.closed)
	}
}

// TestInteractiveMode_NoLineEditor: without a line editor the plain input
// loop takes over.
func TestInteractiveMode_NoLineEditor(t *testing.T) {
	stub := newHome(t)
	stub.SetDefault(stubprovider.Reply("plain answer"))
	loop := newLoop(t)
	stubLineReader(t, func(*readline.Config) (lineReader, error) { return nil, errors.New("no ANSI") })
	var out string
	withStdin(t, "hello\nexit\n", func() {
		out = captureStdout(t, func() { interactiveMode(loop) })
	})
	if !strings.Contains(out, "plain answer") || !strings.Contains(out, "Goodbye!") {
		t.Errorf("output = %q", out)
	}
}
