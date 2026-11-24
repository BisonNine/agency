// Agency
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/logger"
)

// retryClass is what a turn does after a failed model call.
type retryClass int

const (
	// retryAfterBackoff: a timeout, retried once the backoff has passed on
	// the loop's clock.
	retryAfterBackoff retryClass = iota
	// retryAfterCompaction: a context-window overflow, compressed and
	// retried at once.
	retryAfterCompaction
	// noRetry: anything else, the turn fails with the error.
	noRetry
)

// Log messages of the two retry paths.
const (
	msgBackoffRetry    = "Timeout error, retrying after backoff"
	msgCompactionRetry = "Context window error detected, attempting compression"
)

// TestDispatchError_RetryPerClass pins, through a whole turn, what the
// turn does with each kind of model-call error: the buckets the retry loop
// acted on before the spawnllm classifier replaced the local string
// matching.
func TestDispatchError_RetryPerClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want retryClass
	}{
		// Timeouts (retry after backoff), never treated as context errors.
		{"deadline exceeded sentinel", context.DeadlineExceeded, retryAfterBackoff},
		{"wrapped deadline", fmt.Errorf("dispatch: %w", context.DeadlineExceeded), retryAfterBackoff},
		{"client timeout", errors.New("Post \"https://x\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)"), retryAfterBackoff},
		{"timed out", errors.New("request timed out"), retryAfterBackoff},
		{"timeout exceeded", errors.New("timeout exceeded"), retryAfterBackoff},
		{"timeout mentioning context window", errors.New("timed out while assembling context window"), retryAfterBackoff},

		// Context overflow (compress and retry): patterns spawnllm classifies.
		{"context_length_exceeded", errors.New("error: context_length_exceeded"), retryAfterCompaction},
		{"maximum context length", errors.New("This model's maximum context length is 8192 tokens"), retryAfterCompaction},
		{"too many tokens", errors.New("too many tokens in request"), retryAfterCompaction},
		{"prompt is too long", errors.New("prompt is too long: 210000 tokens > 200000 maximum"), retryAfterCompaction},
		{"request too large", errors.New("request too large for model"), retryAfterCompaction},

		// Context overflow: patterns only the local list classifies.
		{"context window", errors.New("exceeds the context window of the model"), retryAfterCompaction},
		{"token limit", errors.New("token limit reached"), retryAfterCompaction},
		{"max_tokens", errors.New("max_tokens exceeds the model limit"), retryAfterCompaction},
		{"invalidparameter", errors.New("InvalidParameter: Total tokens of image and text exceed max message tokens"), retryAfterCompaction},

		// Fallback chain exhausted with every candidate at its context limit.
		{"fallback all context limit", &models.ExhaustedError{Attempts: []models.Attempt{
			{Provider: "p", Model: "m", Err: errors.New("boom"), Reason: spawnllm.FailoverContextLimit},
		}}, retryAfterCompaction},
		{"fallback mixed reasons", &models.ExhaustedError{Attempts: []models.Attempt{
			{Provider: "p", Model: "m", Err: errors.New("boom"), Reason: spawnllm.FailoverAuth},
		}}, noRetry},

		// Neither: the turn fails with the error.
		{"auth", errors.New("401 unauthorized"), noRetry},
		{"canceled", context.Canceled, noRetry},
		{"unknown", errors.New("network is unreachable"), noRetry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &safeBufLoop{}
			t.Cleanup(logger.RedirectForTest(logs))
			var calls atomic.Int32
			failingOnce := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
				if calls.Add(1) == 1 {
					return nil, tt.err
				}
				return answerOK(model), nil
			}}
			run := chainTurn(t, failingOnce, nil, candAlice)
			fc := clock.NewFake(time.Now())
			run.al.clock = fc
			done := make(chan error, 1)
			go func() {
				_, err := run.run(context.Background())
				done <- err
			}()

			if tt.want == retryAfterBackoff {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := fc.BlockUntil(ctx, 1); err != nil {
					t.Fatalf("the retry never waited on the clock: %v", err)
				}
				select {
				case err := <-done:
					t.Fatalf("turn ended before the backoff passed: %v", err)
				default:
				}
				fc.Advance(2 * hook.DefaultRetryBackoff) // past the jittered first step
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("turn did not end")
			}

			out := logs.String()
			backedOff := strings.Contains(out, msgBackoffRetry)
			compacted := strings.Contains(out, msgCompactionRetry)
			switch tt.want {
			case retryAfterBackoff, retryAfterCompaction:
				if err != nil || calls.Load() != 2 {
					t.Fatalf("turn = %v after %d call(s), want success on the retry", err, calls.Load())
				}
				if backedOff != (tt.want == retryAfterBackoff) || compacted != (tt.want == retryAfterCompaction) {
					t.Fatalf("backoff retry logged %v, compaction retry logged %v; want only the one for this error:\n%s",
						backedOff, compacted, out)
				}
			case noRetry:
				if err == nil || calls.Load() != 1 {
					t.Fatalf("turn = %v after %d call(s), want it to fail without a retry", err, calls.Load())
				}
				if !strings.Contains(err.Error(), tt.err.Error()) {
					t.Fatalf("turn error = %q, want it to carry %q", err, tt.err)
				}
				if backedOff || compacted {
					t.Fatalf("a retry was logged for an error that is not retried:\n%s", out)
				}
			}
		})
	}
}

// A model call cut off by the turn budget is not retried, so no retry is
// logged: the budget is spent and the turn ends with the time-limit reply.
func TestTimeoutRetry_NotLoggedWhenBudgetSpent(t *testing.T) {
	logs := &safeBufLoop{}
	t.Cleanup(logger.RedirectForTest(logs))
	cfg := newTestConfig(t)
	cfg.Agents.Defaults.TurnTimeout = 1
	al, msgBus, p := newBlockingLoop(t, cfg)
	fc := fakeClock(al)

	go dispatch(al, inbound("c1", "id1", "hello"))
	p.waitStarted(t)
	advanceWhenWaiting(t, fc, 1, time.Second) // the turn budget runs out
	out := nextOutbound(t, msgBus)
	al.activeRequests.Wait()
	if !strings.Contains(out.Content, "ran past the 1s time limit") {
		t.Fatalf("reply = %q, want the time-limit message", out.Content)
	}
	if strings.Contains(logs.String(), "retrying after backoff") {
		t.Errorf("a retry was logged for a turn whose budget had run out:\n%s", logs.String())
	}
}
