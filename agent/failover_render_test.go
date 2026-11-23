package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
)

func TestAttemptDescriptionIncludesHTTPStatus(t *testing.T) {
	// With a status code, it must be surfaced verbatim (technical users).
	got := attemptDescription("grok-2", 402, spawnllm.FailoverBilling)
	if got != "grok-2 error HTTP 402 (out of credits)" {
		t.Fatalf("got %q", got)
	}
	// Without a status (timeout/network), fall back to the reason — no "HTTP 0".
	got = attemptDescription("gpt-4o", 0, spawnllm.FailoverTimeout)
	if got != "gpt-4o error: timeout" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderFailoverErrorExhausted(t *testing.T) {
	err := &models.ExhaustedError{Attempts: []models.Attempt{
		{
			Provider: "x", Model: "grok-2", Reason: spawnllm.FailoverBilling,
			Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverBilling, Status: 402},
		},
		{
			Provider: "y", Model: "gpt-4o", Reason: spawnllm.FailoverOverloaded,
			Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverOverloaded, Status: 529},
		},
	}}
	out := renderFailoverError(err)
	if !strings.Contains(out, "HTTP 402") || !strings.Contains(out, "grok-2") {
		t.Fatalf("exhausted render missing http code/model: %q", out)
	}
}

func TestRenderFailoverErrorSkipsCooldownOnly(t *testing.T) {
	// All attempts skipped (cooldown) → nothing to render; caller falls through.
	err := &models.ExhaustedError{Attempts: []models.Attempt{
		{Provider: "x", Model: "grok-2", Skipped: true},
	}}
	if out := renderFailoverError(err); out != "" {
		t.Fatalf("expected empty render for all-skipped, got %q", out)
	}
}

// The CLI declined-tools guard's message tells the user what to change, so the
// renderer must show it verbatim wherever the error surfaces: as one attempt of
// an exhausted chain, as a lone FailoverError, in the mid-chain notice, and
// when the chain returns it unclassified (it carries no HTTP status or pattern).
func TestRenderFailoverErrorKeepsCLIDeclinedMessage(t *testing.T) {
	const declinedText = "The Claude CLI declined to use tools. Tick *Bypass CLI restrictions* for this CLI in the WebUI, or allow the tools in the CLI's own settings."
	declined := &models.CLIDeclinedError{Message: declinedText}

	exhausted := &models.ExhaustedError{Attempts: []models.Attempt{
		{
			Provider: "Claude CLI", Model: "claude-cli", Reason: spawnllm.FailoverUnknown,
			Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverUnknown, Model: "claude-cli", Wrapped: declined},
		},
		{
			Provider: "x", Model: "grok-2", Reason: spawnllm.FailoverBilling,
			Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverBilling, Status: 402},
		},
	}}
	out := renderFailoverError(exhausted)
	if !strings.Contains(out, "claude-cli error: "+declinedText) {
		t.Fatalf("exhausted render dropped the declined message: %q", out)
	}
	if !strings.Contains(out, "grok-2 error HTTP 402") {
		t.Fatalf("exhausted render lost the other attempt: %q", out)
	}

	single := &spawnllm.FailoverError{Reason: spawnllm.FailoverUnknown, Model: "claude-cli", Wrapped: declined}
	if out := renderFailoverError(single); out != "claude-cli error: "+declinedText+"." {
		t.Fatalf("single FailoverError render: %q", out)
	}

	notice := formatFallbackNotice(
		[]models.Attempt{{
			Model: "claude-cli", Name: "Claude", Reason: spawnllm.FailoverUnknown,
			Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverUnknown, Wrapped: declined},
		}},
		models.Candidate{Model: "gpt-4o"},
	)
	if !strings.Contains(notice, "Claude error: "+declinedText+".\nTrying gpt-4o…") {
		t.Fatalf("fallback notice dropped the declined message: %q", notice)
	}

	// The cause (agy names the denied action) stays behind the message.
	withCause := fmt.Errorf("fallback: unclassified error from Claude CLI/claude-cli: %w",
		&models.CLIDeclinedError{Message: declinedText, Cause: errors.New("agy denied RunCommand")})
	if out := renderTurnError(context.Background(), time.Minute, withCause); out != declinedText+": agy denied RunCommand" {
		t.Fatalf("unclassified render: %q", out)
	}
}

func TestRenderTurnErrorTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done() // ensure deadline elapsed
	out := renderTurnError(ctx, 15*time.Minute, errors.New("context deadline exceeded"))
	if !strings.Contains(out, "time limit") || !strings.Contains(out, "15m") {
		t.Fatalf("turn-timeout message unexpected: %q", out)
	}
}

func TestFormatFallbackNotice(t *testing.T) {
	failed := models.Attempt{
		Model:  "grok-2",
		Reason: spawnllm.FailoverBilling,
		Err:    &spawnllm.FailoverError{Reason: spawnllm.FailoverBilling, Status: 402},
	}
	next := models.Candidate{Model: "gpt-4o", Name: "smart"}
	got := formatFallbackNotice([]models.Attempt{failed}, next)
	// Alias preferred for the next model; HTTP code surfaced for the failed one;
	// period + newline before "Trying".
	if !strings.Contains(got, "HTTP 402") || !strings.Contains(got, ").\nTrying smart…") {
		t.Fatalf("notice unexpected: %q", got)
	}
}

// A single notifier de-duplicates identical notices across a turn: a primary that
// fails over the same way on every tool iteration posts its heads-up once.
func TestFallbackNotifier_DedupsAcrossTurn(t *testing.T) {
	tl := newTestAgentLoop(t)
	al, msgBus := tl.al, tl.msgBus

	notifier := al.fallbackNotifier(context.Background(), processOptions{Channel: "test", ChatID: "chat"})
	if notifier == nil {
		t.Fatal("expected a notifier")
	}

	collected := make(chan bus.OutboundMessage, 16)
	subCtx := t.Context()
	go func() {
		for {
			msg, ok := msgBus.SubscribeOutbound(subCtx)
			if !ok {
				return
			}
			collected <- msg
		}
	}()

	failA := models.Attempt{
		Model: "deepseek-v4-pro", Name: "DeepSeek 4 Pro",
		Reason: spawnllm.FailoverFormat,
		Err:    &spawnllm.FailoverError{Reason: spawnllm.FailoverFormat, Status: 400},
	}
	nextW := models.Candidate{Model: "deepseek-v4-pro", Name: "DeepSeek V4 Pro Writing"}
	failB := models.Attempt{
		Model: "grok", Name: "Grok",
		Reason: spawnllm.FailoverRateLimit,
		Err:    &spawnllm.FailoverError{Reason: spawnllm.FailoverRateLimit, Status: 429},
	}

	batchA := []models.Attempt{failA}
	batchB := []models.Attempt{failB}
	notifier(batchA, nextW) // published
	notifier(batchA, nextW) // duplicate → suppressed
	notifier(batchA, nextW) // duplicate → suppressed
	notifier(batchB, nextW) // distinct → published

	var seen []bus.OutboundMessage
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case m := <-collected:
			seen = append(seen, m)
		case <-deadline:
			t.Fatalf("timed out; got %d notices, want 2", len(seen))
		}
	}
	// No third notice — the duplicates must have been suppressed.
	if extra := untilMarker(t, msgBus, collected); len(extra) > 0 {
		t.Fatalf("dedup failed; got an extra notice: %q", extra[0].Content)
	}
}

// The notice is posted once per turn per model change: a model in cooldown
// on every step of a turn (its "retry in" time different each time), or one
// that first failed and is then skipped, is announced once; a different
// change still is.
func TestFallbackNotifier_OncePerModelChange(t *testing.T) {
	tl := newTestAgentLoop(t)
	notifier := tl.al.fallbackNotifier(context.Background(), processOptions{Channel: "test", ChatID: "chat"})
	collected := make(chan bus.OutboundMessage, 16)
	go func() {
		for {
			msg, ok := tl.msgBus.SubscribeOutbound(t.Context())
			if !ok {
				return
			}
			collected <- msg
		}
	}()

	luna := models.Candidate{Model: "luna", Name: "Luna"}
	haikuFailed := models.Attempt{
		Model: "haiku", Name: "Haiku", Reason: spawnllm.FailoverFormat,
		Err: &spawnllm.FailoverError{Reason: spawnllm.FailoverFormat, Status: 404},
	}
	notifier([]models.Attempt{haikuFailed}, luna)
	for _, left := range []time.Duration{59 * time.Second, 41 * time.Second, 17 * time.Second, 3 * time.Second} {
		notifier([]models.Attempt{{Model: "haiku", Name: "Haiku", Skipped: true, Reason: spawnllm.FailoverFormat, Remaining: left}}, luna)
	}
	notifier([]models.Attempt{haikuFailed, {Model: "luna", Name: "Luna", Reason: spawnllm.FailoverRateLimit}},
		models.Candidate{Model: "scout", Name: "Scout"})

	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case m := <-collected:
			got = append(got, m.Content)
		case <-deadline:
			t.Fatalf("timed out; got %q, want 2 notices", got)
		}
	}
	if extra := untilMarker(t, tl.msgBus, collected); len(extra) > 0 {
		t.Fatalf("notice repeated for the same model change: %q (earlier %q)", extra[0].Content, got)
	}
	if !strings.Contains(got[0], "Trying Luna") || !strings.Contains(got[1], "Trying Scout") {
		t.Errorf("notices = %q", got)
	}
}

// A single cooldown-skipped candidate names the CAUSE and the retry time (using
// its alias), so the skip is neither silent nor uninformative.
func TestFormatFallbackNotice_Skip(t *testing.T) {
	skipped := models.Attempt{
		Model:     "deepseek-v4-pro",
		Name:      "DeepSeek V4 Pro Writing",
		Skipped:   true,
		Reason:    spawnllm.FailoverRateLimit,
		Remaining: 9 * time.Minute,
	}
	next := models.Candidate{Model: "abliterated-model", Name: "Abliteration"}
	got := formatFallbackNotice([]models.Attempt{skipped}, next)
	want := "⚠️ DeepSeek V4 Pro Writing unavailable — rate limited (retry in 9m). Using Abliteration."
	if got != want {
		t.Fatalf("skip notice:\n got %q\nwant %q", got, want)
	}
}

// A run of skips collapses to ONE line that names the cause — the fix for a
// wall of "skipped (in cooldown)" on every turn while OpenRouter is out of
// credits.
func TestFormatFallbackNotice_SkipBatchCoalesced(t *testing.T) {
	skips := []models.Attempt{
		{
			Model: "deepseek-v4-flash", Name: "OR Chat DeepSeek V4 Flash", Skipped: true,
			Reason: spawnllm.FailoverBilling, Remaining: 41 * time.Minute,
		},
		{
			Model: "deepseek-v4-pro", Name: "OR Chat DeepSeek V4 Pro", Skipped: true,
			Reason: spawnllm.FailoverBilling, Remaining: 58 * time.Minute,
		},
		{
			Model: "glm-5.2", Name: "OR GLM 5.2", Skipped: true,
			Reason: spawnllm.FailoverBilling, Remaining: 12 * time.Minute,
		},
	}
	next := models.Candidate{Model: "grok-medium", Name: "Grok Medium"}
	got := formatFallbackNotice(skips, next)
	want := "⚠️ 3 models unavailable — out of credits (retry in up to 58m). Using Grok Medium."
	if got != want {
		t.Fatalf("batch skip notice:\n got %q\nwant %q", got, want)
	}
}

// Mixed causes across a batch are listed once each, in first-seen order.
func TestFormatFallbackNotice_SkipBatchMixedReasons(t *testing.T) {
	skips := []models.Attempt{
		{Model: "a", Skipped: true, Reason: spawnllm.FailoverBilling, Remaining: time.Minute},
		{Model: "b", Skipped: true, Reason: spawnllm.FailoverRateLimit, Remaining: 2 * time.Minute},
		{Model: "c", Skipped: true, Reason: spawnllm.FailoverBilling, Remaining: time.Minute},
	}
	got := formatFallbackNotice(skips, models.Candidate{Model: "grok"})
	want := "⚠️ 3 models unavailable — out of credits, rate limited (retry in up to 2m). Using grok."
	if got != want {
		t.Fatalf("mixed-reason notice:\n got %q\nwant %q", got, want)
	}
}

// With no recorded reason (and no remaining), the notice degrades to the plain
// "in cooldown" wording rather than inventing a cause.
func TestFormatFallbackNotice_SkipUnknownReason(t *testing.T) {
	skips := []models.Attempt{{Model: "a", Skipped: true}}
	got := formatFallbackNotice(skips, models.Candidate{Model: "grok"})
	want := "⚠️ a unavailable — in cooldown. Using grok."
	if got != want {
		t.Fatalf("unknown-reason notice:\n got %q\nwant %q", got, want)
	}
}

func TestFormatFallbackNotice_Empty(t *testing.T) {
	if got := formatFallbackNotice(nil, models.Candidate{Model: "grok"}); got != "" {
		t.Fatalf("empty batch = %q, want empty", got)
	}
}

func TestFormatRemaining(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Minute, ""},
		{30 * time.Second, "30s"},
		{90 * time.Second, "2m"},
		{42 * time.Minute, "42m"},
		{time.Hour, "60m"},
	}
	for _, c := range cases {
		if got := formatRemaining(c.in); got != c.want {
			t.Errorf("formatRemaining(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// With the assistant named, the declined-tools reply starts with the name so a
// shared channel shows whose CLI refused; other errors are unchanged.
func TestRenderTurnErrorFor_NamesTheAssistant(t *testing.T) {
	declined := &models.CLIDeclinedError{Message: "The Claude CLI declined to use tools (Write)."}
	got := renderTurnErrorFor("Alice", context.Background(), time.Minute, declined)
	if got != "Alice: The Claude CLI declined to use tools (Write)." {
		t.Errorf("got %q", got)
	}
	if got := renderTurnErrorFor("", context.Background(), time.Minute, declined); got != declined.Message {
		t.Errorf("unnamed: got %q", got)
	}
	plain := renderTurnErrorFor("Alice", context.Background(), 15*time.Minute, errors.New("context deadline exceeded"))
	if strings.HasPrefix(plain, "Alice:") {
		t.Errorf("a non-declined error must not be prefixed: %q", plain)
	}
}
