// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// replayProvider answers each wire model from a script (an error text, or
// "" for an answer) and records the wire models it was called with.
type replayProvider struct {
	mu     sync.Mutex
	errs   map[string]string
	called []string
	// onCall, when set, runs before each answer.
	onCall func(model string)
}

func (p *replayProvider) Chat(_ context.Context, _ providers.Identity, _ []providers.Message, _ []providers.ToolDefinition, model string, _ map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.called = append(p.called, model)
	msg := p.errs[model]
	onCall := p.onCall
	p.mu.Unlock()
	if onCall != nil {
		onCall(model)
	}
	if msg != "" {
		return nil, errors.New(msg)
	}
	return &providers.LLMResponse{Content: "from " + model, FinishReason: "stop"}, nil
}

func (p *replayProvider) GetDefaultModel() string { return "" }

func (p *replayProvider) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.called)
}

// attemptSeq renders attempts as today's fallback tests read them: the wire
// model and whether it was skipped (cooldown) or failed, with its reason.
func attemptSeq(attempts []providers.FallbackAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		if a.Skipped {
			out = append(out, a.Model+":skipped")
			continue
		}
		out = append(out, fmt.Sprintf("%s:%s", a.Model, a.Reason))
	}
	return out
}

var (
	replayGPT    = providers.FallbackCandidate{Provider: "openai", Model: "gpt-4", Name: "GPT"}
	replayClaude = providers.FallbackCandidate{Provider: "anthropic", Model: "claude", Name: "Claude"}
	replayLlama  = providers.FallbackCandidate{Provider: "groq", Model: "llama", Name: "Llama"}
)

// TestFallbackReplay replays today's fallback cases through the host's
// wiring (a turn's pinned chain, callModel with its prepare, notify and
// finish hooks): the answering model, the attempts passed over, the notices
// and the models called are those the fallback chain's tests pinned.
func TestFallbackReplay(t *testing.T) {
	tests := []struct {
		name       string
		chain      []providers.FallbackCandidate
		parked     []providers.FallbackCandidate
		errs       map[string]string
		answer     string   // wire model that answers; "" when the call fails
		attempts   []string // passed over (answered) or tried (exhausted)
		notices    [][]string
		calls      []string
		exhausted  bool
		contextAll bool
		otherErr   string // an error that is not an exhausted chain
	}{
		{
			name:     "second candidate answers",
			chain:    []providers.FallbackCandidate{replayGPT, replayClaude},
			errs:     map[string]string{"gpt-4": "rate limit exceeded"},
			answer:   "claude",
			attempts: []string{"gpt-4:rate_limit"},
			notices:  [][]string{{"gpt-4:rate_limit"}},
			calls:    []string{"gpt-4", "claude"},
		},
		{
			name:      "all fail",
			chain:     []providers.FallbackCandidate{replayGPT, replayClaude, replayLlama},
			errs:      map[string]string{"gpt-4": "rate limit exceeded", "claude": "rate limit exceeded", "llama": "rate limit exceeded"},
			attempts:  []string{"gpt-4:rate_limit", "claude:rate_limit", "llama:rate_limit"},
			notices:   [][]string{{"gpt-4:rate_limit"}, {"claude:rate_limit"}},
			calls:     []string{"gpt-4", "claude", "llama"},
			exhausted: true,
		},
		{
			name:     "format error falls back",
			chain:    []providers.FallbackCandidate{replayGPT, replayClaude},
			errs:     map[string]string{"gpt-4": "string should match pattern"},
			answer:   "claude",
			attempts: []string{"gpt-4:format"},
			notices:  [][]string{{"gpt-4:format"}},
			calls:    []string{"gpt-4", "claude"},
		},
		{
			name:     "cooldown skip",
			chain:    []providers.FallbackCandidate{replayGPT, replayClaude},
			parked:   []providers.FallbackCandidate{replayGPT},
			answer:   "claude",
			attempts: []string{"gpt-4:skipped"},
			notices:  [][]string{{"gpt-4:skipped"}},
			calls:    []string{"claude"},
		},
		{
			name:     "consecutive skips coalesce into one notice",
			chain:    []providers.FallbackCandidate{replayGPT, replayClaude, replayLlama},
			parked:   []providers.FallbackCandidate{replayGPT, replayClaude},
			answer:   "llama",
			attempts: []string{"gpt-4:skipped", "claude:skipped"},
			notices:  [][]string{{"gpt-4:skipped", "claude:skipped"}},
			calls:    []string{"llama"},
		},
		{
			name:      "all in cooldown",
			chain:     []providers.FallbackCandidate{replayGPT, replayClaude},
			parked:    []providers.FallbackCandidate{replayGPT, replayClaude},
			attempts:  []string{"gpt-4:skipped", "claude:skipped"},
			exhausted: true,
		},
		{
			name:       "context limit everywhere",
			chain:      []providers.FallbackCandidate{replayGPT, replayClaude},
			errs:       map[string]string{"gpt-4": "status 413: request too large for context window", "claude": "status 413: request too large for context window"},
			attempts:   []string{"gpt-4:context_limit", "claude:context_limit"},
			notices:    [][]string{{"gpt-4:context_limit"}},
			calls:      []string{"gpt-4", "claude"},
			exhausted:  true,
			contextAll: true,
		},
		{
			name:     "unclassified error ends the chain",
			chain:    []providers.FallbackCandidate{replayGPT, replayClaude},
			errs:     map[string]string{"gpt-4": "something odd happened"},
			calls:    []string{"gpt-4"},
			otherErr: "fallback: unclassified error from openai/gpt-4: something odd happened",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(logger.RedirectForTest(&bytes.Buffer{}))
			p := &replayProvider{errs: tt.errs}
			turn, d := chainTurn(t, p, tt.parked, tt.chain...)
			var notices [][]string
			turn.notifier = func(passed []providers.FallbackAttempt, _ providers.FallbackCandidate) {
				notices = append(notices, attemptSeq(passed))
			}
			resp, err := turn.callModel(context.Background(), d)

			switch {
			case tt.answer != "":
				if err != nil || resp.Content != "from "+tt.answer {
					t.Fatalf("callModel = %+v, %v; want %s's answer", resp, err, tt.answer)
				}
			case tt.exhausted:
				ex, ok := errors.AsType[*providers.FallbackExhaustedError](err)
				if !ok {
					t.Fatalf("err = %v, want an exhausted chain", err)
				}
				if got := attemptSeq(ex.Attempts); !slices.Equal(got, tt.attempts) {
					t.Errorf("attempts = %v, want %v", got, tt.attempts)
				}
				if ex.AllContextLimit() != tt.contextAll {
					t.Errorf("AllContextLimit = %v, want %v", ex.AllContextLimit(), tt.contextAll)
				}
			default:
				if err == nil || err.Error() != tt.otherErr {
					t.Fatalf("err = %v, want %q", err, tt.otherErr)
				}
			}
			if fmt.Sprint(notices) != fmt.Sprint(tt.notices) {
				t.Errorf("notices = %v, want %v", notices, tt.notices)
			}
			if got := p.calls(); !slices.Equal(got, tt.calls) {
				t.Errorf("calls = %v, want %v", got, tt.calls)
			}
		})
	}
}

// A cancelled turn stops the chain at once: the next model is not called.
func TestFallbackReplay_ContextCanceled(t *testing.T) {
	t.Cleanup(logger.RedirectForTest(&bytes.Buffer{}))
	ctx, cancel := context.WithCancel(context.Background())
	p := &replayProvider{errs: map[string]string{"gpt-4": "context canceled"}, onCall: func(string) { cancel() }}
	turn, d := chainTurn(t, p, nil, replayGPT, replayClaude)
	_, err := turn.callModel(ctx, d)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := p.calls(); !slices.Equal(got, []string{"gpt-4"}) {
		t.Fatalf("calls = %v, want only gpt-4", got)
	}
}

// A turn is sticky: the chain it pinned when it started keeps its order for
// every step, even when /model changes the session's model mid-turn; the
// next turn starts on the new choice.
func TestFallbackReplay_StickyTurn(t *testing.T) {
	al, agent := fourModelLoop(t)
	p := &replayProvider{}
	for _, name := range al.catalog.Names() {
		if err := al.catalog.RegisterClient(name, p); err != nil {
			t.Fatal(err)
		}
	}
	const session = "sticky"
	first := al.selectTurnModel(agent, session)

	if err := al.setActiveModelIndex(agent, session, 2); err != nil {
		t.Fatal(err)
	}
	turn := &llmTurn{
		al: al, agent: agent, chain: first.chain, candidates: first.candidates, model: first.model,
		messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	for range 2 {
		if _, err := turn.callModel(context.Background(), turn.newDispatch(nil, 0)); err != nil {
			t.Fatal(err)
		}
	}
	next := al.selectTurnModel(agent, session)
	if got := p.calls(); !slices.Equal(got, []string{"m0", "m0"}) {
		t.Fatalf("calls in the turn = %v, want the pinned first model twice", got)
	}
	if next.model != "a2" || next.candidates[0].Model != "m2" {
		t.Fatalf("next turn starts on %s (%s), want a2 (m2)", next.model, next.candidates[0].Model)
	}
	if strings.Join(candidateNamesOf(next.candidates), ",") != "a2,a0,a1,a3" {
		t.Fatalf("next turn order = %v", candidateNamesOf(next.candidates))
	}
}

func candidateNamesOf(cands []providers.FallbackCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Name)
	}
	return out
}
