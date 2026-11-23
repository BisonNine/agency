package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/common"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// modelScriptProvider answers each call by the wire model it is sent.
type modelScriptProvider struct {
	answer func(ctx context.Context, model string) (*spawnllm.LLMResponse, error)
}

func (p modelScriptProvider) Chat(ctx context.Context, _ accounting.Identity, _ []spawnllm.Message, _ []spawnllm.ToolDefinition, model string, _ map[string]any) (*spawnllm.LLMResponse, error) {
	return p.answer(ctx, model)
}

func (modelScriptProvider) GetDefaultModel() string { return "alice-wire" }

var (
	candOpus  = models.Candidate{Provider: "Anthropic", Model: "opus-wire", Name: "Claude Opus"}
	candBob   = models.Candidate{Provider: "Bob's API", Model: "bob-wire", Name: "Bob Model"}
	candAlice = models.Candidate{Provider: "Alice's API", Model: "alice-wire", Name: "Alice Model"}
)

const rateLimited = "status 429: rate limit exceeded"

// answerOK is a successful answer naming the served model.
func answerOK(model string) *spawnllm.LLMResponse {
	return &spawnllm.LLMResponse{
		Content: "from " + model,
		Status:  &spawnllm.DispatchStatus{Success: true, Model: model + "-2026"},
	}
}

// chainCatalog is a catalogue holding the candidates (each on its provider,
// p answering for all of them), with the parked ones parked by a rate-limit
// failure, as a turn's chain would have left them.
func chainCatalog(t *testing.T, p spawnllm.LLMProvider, parked []models.Candidate, candidates ...models.Candidate) *models.Catalog {
	t.Helper()
	cfg := &config.Config{}
	clients := map[string]spawnllm.LLMProvider{}
	for _, c := range append(slices.Clone(candidates), parked...) {
		if _, ok := clients[c.Name]; ok {
			continue
		}
		if _, err := cfg.GetProvider(c.Provider); err != nil {
			cfg.Providers = append(cfg.Providers, modelspec.Provider{Name: c.Provider, Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"})
		}
		cfg.Models = append(cfg.Models, modelspec.Model{ModelName: c.Name, Model: c.Model, Provider: c.Provider, Enabled: true})
		clients[c.Name] = p
	}
	cat := testCatalog(cfg, nil)
	for _, c := range parked {
		failing := modelstest.New().Script(c.Model, modelstest.Status(429, 0))
		if err := cat.RegisterClient(c.Name, failing); err != nil {
			t.Fatal(err)
		}
		if _, err := providers.ModelSet(cat, c.Name).Begin(models.Chat).Chat(context.Background(), testIdentity, models.Request{}); err == nil {
			t.Fatalf("the failing call to %s succeeded", c.Name)
		}
		if info, _ := cat.Get(c.Name); info.Health.Available {
			t.Fatalf("%s is not parked", c.Name)
		}
	}
	for name, cl := range clients {
		if err := cat.RegisterClient(name, cl); err != nil {
			t.Fatal(err)
		}
	}
	return cat
}

// chainRun is a turn of alice whose chain holds candidates, in order,
// answered by p, with the parked ones in cooldown; run runs it whole.
type chainRun struct {
	t     *testing.T
	al    *AgentLoop
	agent *AgentInstance
	tm    *turnModel
	// plugins are added after the turn's own.
	plugins []hook.Plugin
}

// chainTurn is the chainRun of candidates answered by p, parked parked.
func chainTurn(t *testing.T, p spawnllm.LLMProvider, parked []models.Candidate, candidates ...models.Candidate) *chainRun {
	t.Helper()
	cat := chainCatalog(t, p, parked, candidates...)
	set := cat.Set("alice")
	for i, c := range candidates {
		if err := set.Assign(models.Chat, c.Name, i); err != nil {
			t.Fatal(err)
		}
	}
	chain := set.Begin(models.Chat)
	al := &AgentLoop{cfg: &config.Config{}, catalog: cat}
	return &chainRun{
		t: t, al: al, agent: &AgentInstance{ID: "alice", MaxIterations: 1},
		tm: &turnModel{chain: chain, candidates: chain.Candidates(), model: candidates[0].Name},
	}
}

// run runs the turn ("hi") on ctx and returns its reply and error.
func (c *chainRun) run(ctx context.Context) (string, error) {
	c.t.Helper()
	_, _, reply, err := runTestTurn(c.t, ctx, c.al, c.agent, processOptions{}, c.tm, nil, c.plugins, nil)
	return reply, err
}

// dispatch runs the turn with no retry and returns what its one dispatch's
// walk of the chain returned: the answer's content, or the error the walk
// ended with (the turn's own when ctx ended first).
func (c *chainRun) dispatch(ctx context.Context) (string, error) {
	c.t.Helper()
	var walkErr error
	walk := hook.Plugin{Name: "walk", OnModelError: func(_ context.Context, _ *hook.ModelInfo, err error) (*spawnllm.LLMResponse, error) {
		walkErr = err
		return &spawnllm.LLMResponse{Content: "walk ended"}, nil
	}}
	noRetry := func(s *loop.Spec) {
		limits := s.TurnLimits
		s.TurnLimits = func(ctx context.Context, sc hook.Scope) hook.Limits {
			l := limits(ctx, sc)
			l.Retries = -1 // none
			return l
		}
	}
	_, _, reply, err := runTestTurn(c.t, ctx, c.al, c.agent, processOptions{}, c.tm, nil, append(slices.Clone(c.plugins), walk), noRetry)
	if walkErr != nil {
		return "", walkErr
	}
	return reply, err
}

// Log messages the per-call contract is about.
const (
	msgFinish    = "LLM finish"
	msgFailed    = "Fallback: candidate failed"
	msgSkipped   = "Fallback: skipped candidate (cooldown)"
	msgCallError = "LLM call failed"
)

// callRecords returns, in order, the finish, fallback and call-failed records
// in buf.
func callRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range readLogRecords(t, buf) {
		msg := fmt.Sprint(rec["message"])
		if msg == msgFinish || msg == msgCallError || strings.HasPrefix(msg, "Fallback: ") {
			out = append(out, rec)
		}
	}
	return out
}

// assertSequence fails unless recs carry exactly the messages want, in order.
func assertSequence(t *testing.T, recs []map[string]any, want ...string) {
	t.Helper()
	got := make([]string, len(recs))
	for i, rec := range recs {
		got[i] = fmt.Sprint(rec["message"])
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log sequence = %q, want %q", got, want)
	}
}

// A candidate that fails gets its own finish line and its WARN at once,
// before the next candidate's line; the one that answers gets its own.
func TestCallModel_FailedCandidateLoggedAsItFails(t *testing.T) {
	p := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
		if model == candOpus.Model {
			return nil, errors.New(anthropic400)
		}
		return answerOK(model), nil
	}}
	run := chainTurn(t, p, nil, candOpus, candBob)

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	resp, err := run.run(context.Background())
	if err != nil {
		t.Fatalf("requestModel: %v", err)
	}
	if resp != "from bob-wire" {
		t.Fatalf("content = %q, want Bob's answer", resp)
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish, msgFailed, msgFinish, "Fallback: succeeded with Bob's API/bob-wire after 2 attempts")
	assertFields(t, recs[0], map[string]any{
		"our_provider": "Anthropic",
		"our_model":    "Claude Opus",
		"model":        "opus-wire",
		"success":      false,
	})
	if e := fmt.Sprint(recs[0]["error"]); !strings.Contains(e, "thinking.type: adaptive is required") {
		t.Errorf("finish error = %q, want the provider's message", e)
	}
	assertFields(t, recs[1], map[string]any{
		"level":    "warn",
		"provider": "Anthropic",
		"model":    "opus-wire",
		"reason":   string(spawnllm.FailoverFormat),
	})
	if e := fmt.Sprint(recs[1]["error"]); !strings.Contains(e, "thinking.type: adaptive is required") {
		t.Errorf("WARN error = %q, want the provider's message", e)
	}
	assertFields(t, recs[2], map[string]any{
		"our_provider": "Bob's API",
		"our_model":    "Bob Model",
		"model":        "bob-wire-2026",
		"success":      true,
	}, "error")
	assertFields(t, recs[3], map[string]any{"level": "info"})
}

// A call cut off by shutdown gets its finish line, naming the candidate that
// was running, and no candidate-failed WARN; the failure before it keeps its
// lines.
func TestCallModel_CancelledCallLogsRunningCandidate(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p := modelScriptProvider{answer: func(ctx context.Context, model string) (*spawnllm.LLMResponse, error) {
		if model == candOpus.Model {
			return nil, errors.New(rateLimited)
		}
		cancel(errShuttingDown)
		return nil, ctx.Err()
	}}
	run := chainTurn(t, p, nil, candOpus, candBob)

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := run.run(ctx); !errors.Is(err, errShuttingDown) {
		t.Fatalf("requestModel error = %v, want the shutdown interruption", err)
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish, msgFailed, msgFinish)
	assertFields(t, recs[0], map[string]any{"our_model": "Claude Opus", "success": false, "error": rateLimited})
	assertFields(t, recs[1], map[string]any{"provider": "Anthropic", "model": "opus-wire"})
	assertFields(t, recs[2], map[string]any{
		"our_provider": "Bob's API",
		"our_model":    "Bob Model",
		"success":      false,
		"error":        "context canceled",
	})
}

// When every candidate fails, each call has its finish line; only the ones
// the chain fell back from get a WARN, not the last; the turn's failure is
// logged once.
func TestCallModel_AllCandidatesFail(t *testing.T) {
	p := modelScriptProvider{answer: func(context.Context, string) (*spawnllm.LLMResponse, error) {
		return nil, errors.New(rateLimited)
	}}
	run := chainTurn(t, p, nil, candOpus, candBob)

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := run.run(context.Background()); err == nil {
		t.Fatal("requestModel succeeded, want an error")
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish, msgFailed, msgFinish, msgCallError)
	assertFields(t, recs[0], map[string]any{"our_model": "Claude Opus", "success": false, "error": rateLimited})
	assertFields(t, recs[1], map[string]any{"model": "opus-wire", "reason": string(spawnllm.FailoverRateLimit)})
	assertFields(t, recs[2], map[string]any{"our_model": "Bob Model", "success": false, "error": rateLimited})
	assertFields(t, recs[3], map[string]any{"level": "error"})
}

// A candidate in cooldown is not called: it has no finish line, and its skip
// is logged once, wherever it sits in the chain.
func TestCallModel_CooldownSkipLoggedOnceWithoutFinish(t *testing.T) {
	tests := []struct {
		name    string
		parked  []models.Candidate
		failing map[string]bool
		wantErr bool
		want    []string
	}{
		{
			name:   "skip then answer",
			parked: []models.Candidate{candOpus},
			want:   []string{msgSkipped, msgFinish, "Fallback: succeeded with Bob's API/bob-wire after 2 attempts"},
		},
		{
			name:    "failure then trailing skip",
			parked:  []models.Candidate{candBob},
			failing: map[string]bool{candOpus.Model: true},
			wantErr: true,
			want:    []string{msgFinish, msgFailed, msgSkipped, msgCallError},
		},
		{
			name:    "all skipped",
			parked:  []models.Candidate{candOpus, candBob},
			wantErr: true,
			want:    []string{msgSkipped, msgSkipped, msgCallError},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
				if tt.failing[model] {
					return nil, errors.New(rateLimited)
				}
				return answerOK(model), nil
			}}
			run := chainTurn(t, p, tt.parked, candOpus, candBob)

			buf := &bytes.Buffer{}
			t.Cleanup(logger.RedirectForTest(buf))
			_, err := run.run(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("requestModel error = %v, want error %v", err, tt.wantErr)
			}

			recs := callRecords(t, buf)
			assertSequence(t, recs, tt.want...)
			parked := map[string]bool{}
			for _, c := range tt.parked {
				parked[c.Model] = true
			}
			for _, rec := range recs {
				switch rec["message"] {
				case msgFinish:
					for _, c := range tt.parked {
						if rec["our_model"] == c.Name {
							t.Errorf("finish line for parked %s", c.Name)
						}
					}
				case msgSkipped:
					if !parked[fmt.Sprint(rec["model"])] {
						t.Errorf("skip logged for %v, which is not parked", rec["model"])
					}
				}
			}
		})
	}
}

// A chain of one model that answers writes exactly one line.
func TestCallModel_DirectPathOneFinish(t *testing.T) {
	p := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
		return answerOK(model), nil
	}}
	run := chainTurn(t, p, nil, models.Candidate{Provider: "Alice Co", Model: "alice-wire", Name: "Alice Model"})

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := run.run(context.Background()); err != nil {
		t.Fatalf("requestModel: %v", err)
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish)
	assertFields(t, recs[0], map[string]any{
		"our_provider": "Alice Co",
		"our_model":    "Alice Model",
		"model":        "alice-wire-2026",
		"success":      true,
	})
}

// A context-window retry writes one finish line per call: the call that
// overflowed and the retried one.
func TestRequestModel_ContextWindowRetryOneFinishPerCall(t *testing.T) {
	al := newTestAgentLoop(t).al
	agent := al.registry.Default()
	if agent == nil {
		t.Fatal("no default agent")
	}
	const sessionKey = "ctx-window-finish"
	for range 6 {
		addMessage(t, agent.Sessions, sessionKey, "user", "old user msg")
		addMessage(t, agent.Sessions, sessionKey, "assistant", "old assistant reply")
	}
	setTestProvider(agent, &sequenceProvider{
		responses: []*spawnllm.LLMResponse{nil, {Content: "recovered"}},
		errors:    []error{errors.New("context_length_exceeded: request too long"), nil},
	})
	opts := processOptions{SessionKey: sessionKey, Channel: "cli", ChatID: "direct", UserMessage: "trigger"}
	cm, releaseCM := al.getContextManager(agent, sessionKey)
	defer releaseCM()

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if got := runIteration(t, al, agent, []spawnllm.Message{{Role: "user", Content: "trigger"}}, opts, cm).content; got != "recovered" {
		t.Fatalf("content = %q, want the retried answer", got)
	}

	var agentCalls []map[string]any
	for _, rec := range finishRecords(t, buf) {
		if rec["kind"] == llmKindAgent {
			agentCalls = append(agentCalls, rec)
		}
	}
	if len(agentCalls) != 2 {
		t.Fatalf("agent finish lines = %d, want 2: %v", len(agentCalls), agentCalls)
	}
	assertFields(t, agentCalls[0], map[string]any{"success": false, "error": "context_length_exceeded: request too long"})
	assertFields(t, agentCalls[1], map[string]any{"success": true}, "error")
}

// failSkipAnswer is a turn whose chain fails Opus, skips Bob (in cooldown)
// and is answered by Alice's model.
func failSkipAnswer(t *testing.T) *chainRun {
	t.Helper()
	p := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
		if model == candOpus.Model {
			return nil, errors.New(rateLimited)
		}
		return answerOK(model), nil
	}}
	return chainTurn(t, p, []models.Candidate{candBob}, candOpus, candBob, candAlice)
}

// The turn's own notifier still hears of each candidate passed over: the
// failure and the skip, each with the candidate tried next.
func TestCallModel_ForwardsToTurnNotifier(t *testing.T) {
	run := failSkipAnswer(t)
	type notice struct {
		passed []models.Attempt
		next   models.Candidate
	}
	var notices []notice
	run.plugins = append(run.plugins, hook.Plugin{Name: "test", OnFallback: func(_ context.Context, ev hook.Fallback) {
		notices = append(notices, notice{passed: fallbackAttempts(ev.Passed), next: fallbackCandidate(ev.Next)})
	}})
	t.Cleanup(logger.RedirectForTest(&bytes.Buffer{}))
	if _, err := run.run(context.Background()); err != nil {
		t.Fatalf("requestModel: %v", err)
	}

	if len(notices) != 2 {
		t.Fatalf("notices = %d, want 2: %+v", len(notices), notices)
	}
	first, second := notices[0], notices[1]
	if len(first.passed) != 1 || first.passed[0].Skipped || first.passed[0].Model != candOpus.Model || first.next.Name != candBob.Name {
		t.Errorf("first notice = %+v, want Opus's failure then Bob", first)
	}
	if len(second.passed) != 1 || !second.passed[0].Skipped || second.passed[0].Model != candBob.Model || second.next.Name != candAlice.Name {
		t.Errorf("second notice = %+v, want Bob's skip then Alice", second)
	}
}

// Fail, skip, answer: finish lines for the two calls only, one WARN for the
// failure and one skip line, in the order they happened.
func TestCallModel_FailSkipAnswerOrder(t *testing.T) {
	run := failSkipAnswer(t)
	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := run.run(context.Background()); err != nil {
		t.Fatalf("requestModel: %v", err)
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish, msgFailed, msgSkipped, msgFinish,
		"Fallback: succeeded with Alice's API/alice-wire after 3 attempts")
	assertFields(t, recs[0], map[string]any{"our_model": "Claude Opus", "success": false})
	assertFields(t, recs[1], map[string]any{"model": "opus-wire"})
	assertFields(t, recs[2], map[string]any{"model": "bob-wire"})
	assertFields(t, recs[3], map[string]any{"our_model": "Alice Model", "success": true})
}

// A same-model retry after a short Retry-After is not a fallback: both calls
// have a finish line, and no candidate-failed WARN is written.
func TestCallModel_SameModelRetryNoWarn(t *testing.T) {
	calls := 0
	p := modelScriptProvider{answer: func(_ context.Context, model string) (*spawnllm.LLMResponse, error) {
		calls++
		if calls == 1 {
			return nil, &common.HTTPStatusError{StatusCode: 429, BodyPreview: "slow down", RetryAfter: time.Millisecond}
		}
		return answerOK(model), nil
	}}
	run := chainTurn(t, p, nil, candOpus, candBob)

	buf := &bytes.Buffer{}
	t.Cleanup(logger.RedirectForTest(buf))
	if _, err := run.run(context.Background()); err != nil {
		t.Fatalf("requestModel: %v", err)
	}

	recs := callRecords(t, buf)
	assertSequence(t, recs, msgFinish, msgFinish)
	assertFields(t, recs[0], map[string]any{"our_model": "Claude Opus", "success": false})
	assertFields(t, recs[1], map[string]any{"our_model": "Claude Opus", "success": true}, "error")
}
