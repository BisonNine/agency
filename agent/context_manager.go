// ClawEh
// License: MIT

package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/logger"
)

// compressModelCaller is the host's hook.Caller for compaction: an adapter over
// the agent's set's Caller for summaries (models.Summarize) or memory
// consolidation (models.Memory). The set's list is the agent's
// summarization chain (agent summarization_models → global
// summarization.models → the agent's primary model); the catalogue's walk
// skips models the engine excluded and models it has parked, moves on after
// any failure (recording one spawnllm classifies against the model's
// health, the one every path shares), and returns the first reply with the
// model that produced it. The host writes the finish line of every call and
// the skip and failure lines itself, as it always has.
type compressModelCaller struct {
	// set is the agent's set; nil when the agent has no summarization chain
	// (a human agent).
	set     *models.Set
	purpose models.Purpose
	scope   models.Scope
	// workspace, when set, runs a CLI model isolated in it (a fresh
	// temporary agent's).
	workspace string
	// caller and sessionKey label the log lines.
	caller     llmCaller
	sessionKey string
	// id is who the calls are billed to: the agent whose conversation or
	// memory is being summarized.
	id accounting.Identity
	// noSessionKey leaves session_key off the finish line, for a caller whose
	// sessionKey names no conversation (memory consolidation).
	noSessionKey bool
}

// Complete implements hook.Caller.
func (c *compressModelCaller) Complete(ctx context.Context, req hook.CompleteRequest) (hook.CompleteReply, error) {
	content, finishReason, model, err := c.complete(ctx, req.System, req.User, req.JSONObject, req.Exclude)
	return hook.CompleteReply{Content: content, FinishReason: finishReason, Model: model}, err
}

// noSummaryModelError is the error of a summary call no model could serve:
// it wraps hook.ErrNoModelAvailable and reads as it always has, "no
// summarization model available: " and the detail.
type noSummaryModelError struct{ detail string }

func (e *noSummaryModelError) Error() string {
	return "no summarization model available: " + e.detail
}

func (e *noSummaryModelError) Unwrap() error { return hook.ErrNoModelAvailable }

// complete makes the call, in plain types so another caller shape (the
// cognitive-memory consolidation model) can wrap it without an import. It
// returns the reply and the model that served it; on failure the model is
// the last one tried (or "" when nothing was), so a report can still name it.
// When no model could be called the error wraps hook.ErrNoModelAvailable; when
// ctx ends the walk stops with ctx's error.
func (c *compressModelCaller) complete(ctx context.Context, system, user string, jsonObject bool, exclude []string) (content, finishReason, model string, err error) {
	if c.set == nil {
		return "", "", "", &noSummaryModelError{detail: "0 excluded, 0 in cooldown"}
	}
	chain := c.set.Begin(c.purpose, c.scope)
	if c.workspace != "" {
		chain = chain.Isolated(c.workspace)
	}
	sessionKey := c.sessionKey
	if c.noSessionKey {
		sessionKey = ""
	}
	finish := models.WithFinish(func(cand models.Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error) {
		emitLLMFinishEvent(llmFinish{
			caller:         c.caller,
			kind:           llmKindCompression,
			ourProvider:    cand.Provider,
			ourModel:       summaryModelName(cand),
			requestedModel: cand.Model,
			sessionKey:     sessionKey,
			turnID:         turnIDFrom(ctx),
			elapsedMs:      elapsed.Milliseconds(),
			resp:           resp,
			err:            err,
		})
	})
	notify := models.WithNotify(func(passed []models.Attempt, _ models.Candidate) {
		for _, a := range passed {
			switch {
			case a.Disabled:
			case a.Skipped:
				logger.InfoCF("llmcontext", "compression model in cooldown; skipping", map[string]any{
					"agent_id":  c.caller.agentID,
					"session":   c.sessionKey,
					"model":     a.Model,
					"remaining": a.Remaining.Round(time.Second).String(),
				})
			default:
				logger.WarnCF("llmcontext", "compression model call failed; trying next in chain", map[string]any{
					"agent_id": c.caller.agentID,
					"session":  c.sessionKey,
					"model":    a.Model,
					"error":    redactedError(a.Err),
				})
			}
		}
	})
	reply, err := chain.Caller(c.id, finish, notify).Complete(ctx, models.CompleteRequest{
		System: system, User: user, JSONObject: jsonObject, Exclude: exclude,
	})
	if err != nil {
		if nme, ok := errors.AsType[*models.NoModelError](err); ok {
			return "", "", "", &noSummaryModelError{detail: nme.Detail}
		}
		return "", "", reply.Model, err
	}
	return reply.Content, reply.FinishReason, reply.Model, nil
}

// resolveCompressModelChain returns the ordered, de-duplicated summarization
// model chain for an agent: its own summarization_models first, then the global
// summarization.models. Blank entries are skipped; the first occurrence of each
// model name wins. An entry that names no enabled model is skipped later
// (summarizationNames); the agent's primary chat model is appended there.
func resolveCompressModelChain(agentModels, globalModels []string) []string {
	seen := make(map[string]struct{}, len(agentModels)+len(globalModels))
	var out []string
	for _, list := range [][]string{agentModels, globalModels} {
		for _, raw := range list {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// compressProviderName resolves the provider name of a model alias, for the
// finish line.
func compressProviderName(cfg *config.Config, alias string) string {
	if cfg == nil {
		return ""
	}
	if mc, err := cfg.GetModelConfig(alias); err == nil && mc != nil {
		return mc.Provider
	}
	return ""
}

// resolveCompressModelTarget resolves a configured model reference (a
// summarization or vision model) into the model_name and wire model id of
// the enabled models entry it names: by model_name first, then by raw model
// id. ok is false when it names none.
func resolveCompressModelTarget(cfg *config.Config, raw string) (alias, modelID string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || cfg == nil {
		return "", "", false
	}

	// Direct alias / ModelName lookup wins: this lets users say
	// compress_model: "haiku" and have it resolve to the models entry
	// whose model_name == "haiku".
	if mc, err := cfg.GetModelConfig(raw); err == nil && mc != nil && mc.Model != "" {
		return mc.ModelName, mc.Model, true
	}

	// Otherwise scan enabled models entries: match by the raw model id.
	for i := range cfg.Models {
		if !cfg.Models[i].Enabled {
			continue
		}
		if strings.TrimSpace(cfg.Models[i].Model) == raw {
			return cfg.Models[i].ModelName, cfg.Models[i].Model, true
		}
	}

	return "", "", false
}

// logCompressModel logs how one configured summarization model resolves, and
// reports whether it does: a name that names no enabled model is skipped
// (summarizationNames), and said so.
func (al *AgentLoop) logCompressModel(agent *AgentInstance, compressModelName, sessionKey string) bool {
	alias, modelID, ok := resolveCompressModelTarget(al.GetConfig(), compressModelName)
	if !ok {
		logger.WarnCF("llmcontext", "compression model not found in enabled models; skipping", map[string]any{
			"agent_id":  agent.ID,
			"requested": compressModelName,
			"session":   sessionKey,
		})
		return false
	}
	logger.DebugCF("llmcontext", "compression model resolved", map[string]any{
		"agent_id":  agent.ID,
		"requested": compressModelName,
		"alias":     alias,
		"model":     modelID,
		"session":   sessionKey,
	})
	return true
}

// newCompressModelCaller builds the summarization ModelCaller for an agent and
// session: the agent's own summarization_models first, then the global
// cfg.Summarization.Models (each skipped when it names no enabled model),
// then the agent's primary chat model, so summarization still works when the
// lists are empty or every configured model fails; an agent with no chat
// model has none. Health is the catalogue's, shared with every other
// path, so a model parked by any path (e.g. an out-of-credits 402) is skipped by
// both. Also returns the effective model name for the summary stamp: the
// first configured entry that is used, else the primary chat model.
func (al *AgentLoop) newCompressModelCaller(agent *AgentInstance, sessionKey string, purpose models.Purpose) (*compressModelCaller, string) {
	if agent.HumanModel != "" {
		// A person's conversation is never given to a model: no
		// summarization chain at all, so compaction and consolidation find
		// no model (and the person is never asked to summarize).
		return &compressModelCaller{caller: callerOf(agent), sessionKey: sessionKey, id: agent.Identity()}, ""
	}
	var agentModels, globalModels []string
	if agent.Config != nil {
		agentModels = agent.Config.SummarizationModels
	}
	if cfg := al.GetConfig(); cfg != nil {
		globalModels = cfg.Summarization.Models
	}

	var used []string
	for _, name := range resolveCompressModelChain(agentModels, globalModels) {
		if al.logCompressModel(agent, name, sessionKey) {
			used = append(used, name)
		}
	}
	chain := slices.Clone(used)
	primary := ""
	if list := al.catalog.Set(agent.ID).List(models.Chat); len(list) > 0 {
		primary = list[0].Name
		chain = append(chain, primary+" (agent default)")
	}

	// One clear line (in claw.log) showing the whole compression chain that will
	// be tried in order — the per-client detail above goes to the structured log
	// too, but this is the at-a-glance summary of what's actually in effect.
	logger.InfoCF("llmcontext", "compression model chain", map[string]any{
		"agent_id": agent.ID,
		"session":  sessionKey,
		"chain":    chain,
	})

	// The rendered "Generated: <time> by <model>" line names the effective
	// compress model; it is the agent's primary chat model when no
	// summarization model is used.
	effective := primary
	if len(used) > 0 {
		effective = used[0]
	}
	// The chain is the agent's set's list for the purpose (assignAgentModels):
	// the summarization models in order, then the primary model unless listed.
	c := &compressModelCaller{
		set:        al.catalog.Set(agent.ID),
		purpose:    purpose,
		scope:      models.Scope(sessionKey),
		caller:     callerOf(agent),
		sessionKey: sessionKey,
		id:         agent.Identity(),
	}
	if agent.Spec.Fresh {
		c.workspace = agent.Workspace
	}
	return c, effective
}

// getContextManager returns the ContextManager for the given agent+session
// pair. See getSessionContext; this is the form for callers that do not touch
// memory (compact, clear, session info).
func (al *AgentLoop) getContextManager(agent *AgentInstance, sessionKey string) (ctxengine.ContextManager, func()) {
	cm, _, release := al.getSessionContext(agent, sessionKey)
	return cm, release
}

// getSessionContext returns the ContextManager and the cognitive-memory session
// for the given agent+session pair, creating and caching them on first access.
// Both are shared across all calls for the same (agentID, sessionKey) tuple;
// the memory session is nil for agents without cognitive memory.
//
// The returned release function must be deferred by the caller to decrement the
// reference count. The eviction goroutine skips entries with refcount > 0.
func (al *AgentLoop) getSessionContext(agent *AgentInstance, sessionKey string) (ctxengine.ContextManager, *cogmem.Session, func()) {
	key := agent.ID + ":" + sessionKey

	for {
		// Fast path: entry already exists. A stale entry (marked by a config
		// reload) is only handed out under the build slot below, where the
		// refcount decides between sharing it and rebuilding it. The reference
		// is taken before the mark is checked — the reverse of
		// tryEvictEntry — so an evictor either counts this caller or
		// is seen by it. An entry already marked is skipped without touching
		// the refcount: a transient reference would look like a holder to
		// takeCachedEntry and have it share a released stale entry instead of
		// rebuilding it.
		v, _ := al.contextManagers.Load(key)
		if entry, ok := v.(*cmEntry); ok && !entry.stale.Load() {
			entry.refcount.Add(1)
			if !entry.stale.Load() {
				entry.touch()
				release := func() { entry.refcount.Add(-1) }
				return entry.cm, entry.mem, release
			}
			entry.refcount.Add(-1)
		}

		// Slow path: exactly one caller builds the entry; everyone else waits
		// for it and then takes the fast path. Issuing a session token rotates
		// the session's previous one, so two concurrent builders would each
		// issue and the loser's Revoke (or its own later Issue) would kill the
		// token the winner had just rendered into its prompt.
		bk := sessionBuildKey{al: al, key: key}
		done := make(chan struct{})
		if inflight, loaded := sessionBuilds.LoadOrStore(bk, done); loaded {
			// sessionBuilds only ever holds the done channels stored here.
			if ch, ok := inflight.(chan struct{}); ok {
				<-ch
			}
			continue
		}
		entry := al.buildSessionEntry(bk, done, agent, sessionKey)
		release := func() { entry.refcount.Add(-1) }
		return entry.cm, entry.mem, release
	}
}

// takeCachedEntry is the build-slot holder's second look at the cache. A live
// entry — or a stale one still held by an in-flight turn, whose token running
// sub-agents carry — is shared: its refcount is taken and it is returned. A
// stale entry nobody holds is evicted so the caller builds a fresh one from the
// current config. The caller holds the build slot for key; since the fast path
// never uses a stale entry (it backs its reference out when it sees the mark),
// nobody can start using one between the refcount check here and its eviction.
// A fast-path caller's transient reference (possible only while an evictor is
// marking the entry at that moment) can only make this share an entry that is
// still open, never evict one in use. Every other evictor also holds
// the slot (tryEvictEntry), so none can race this one.
func (al *AgentLoop) takeCachedEntry(key string) (*cmEntry, bool) {
	v, _ := al.contextManagers.Load(key)
	entry, ok := v.(*cmEntry)
	if !ok {
		return nil, false
	}
	if entry.stale.Load() && entry.refcount.Load() == 0 {
		al.evictEntry(context.Background(), key, entry, evictReasonStaleRebuild)
		return nil, false
	}
	entry.refcount.Add(1)
	entry.touch()
	return entry, true
}

// sessionBuildKey identifies one in-flight context-manager build: the loop it
// belongs to and the agentID:sessionKey being built.
type sessionBuildKey struct {
	al  *AgentLoop
	key string
}

// sessionBuilds holds the in-flight builds of getSessionContext: sessionBuildKey
// to a channel closed once the entry is in contextManagers. An entry exists only
// for the duration of one build.
var sessionBuilds sync.Map

// buildSessionEntry creates the ContextManager, session token and cognitive
// memory session for one agent+session pair, stores the entry in
// contextManagers with refcount 1 (the caller's reference), and then releases
// the waiters registered under bk. The caller holds the build slot for bk.
// When the cache already holds an entry the caller may share (a stale one still
// in use, see takeCachedEntry), that entry is returned with the caller's
// reference taken instead of building a new one.
func (al *AgentLoop) buildSessionEntry(bk sessionBuildKey, done chan struct{}, agent *AgentInstance, sessionKey string) *cmEntry {
	// Release waiters even if the build panics, so they retry instead of
	// hanging; finding no entry, one of them becomes the builder.
	defer func() {
		sessionBuilds.Delete(bk)
		close(done)
	}()

	if entry, ok := al.takeCachedEntry(bk.key); ok {
		return entry
	}

	// The summarization model chain. Per-agent models let an agent use
	// specialised summarizers when the default ones refuse its content; see
	// summarizationNames for the chain's rules.
	caller, effectiveCompressModel := al.newCompressModelCaller(agent, sessionKey, models.Summarize)

	// Global debug-capture flag: when on, the manager writes the verbatim
	// request/response of each summarization call to <workspace>/compact.jsonl.
	// Failed summarization attempts are dumped to logs/dumps when enabled.
	debugCapture := false
	var failureDump ctxengine.FailureDumpFunc
	if cfg := al.GetConfig(); cfg != nil {
		debugCapture = cfg.Summarization.DebugCapture
		if cfg.Logging.DumpFailedCompressions && al.dumpsDir.Path != "" {
			dumpsDir := al.dumpsDir
			failureDump = func(kind string, meta map[string]any, input, output string) error {
				_, err := dump.Write(dumpsDir, kind, meta, json.RawMessage(input), json.RawMessage(output))
				return err
			}
		}
	}

	// Reporter delivers the compaction report to the user on the automatic path.
	// Internal channels (e.g. cron-internal) are skipped to avoid loops; manual
	// /compact returns the report directly instead of using this.
	reporter := func(channel, chatID, text string) {
		if text == "" || channel == "" || al.bus == nil || constants.IsInternalChannel(channel) {
			return
		}
		if err := al.bus.PublishOutbound(context.Background(), bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: text,
		}); err != nil {
			logger.WarnCF("llmcontext", "Failed to deliver compaction report", map[string]any{
				"channel": channel,
				"error":   err.Error(),
			})
		}
	}

	// The archive directory is the sessions directory within the agent's state
	// directory, derived the same way initSessionStore does.
	archiveDir := agent.SessionsDir()
	// A fresh temporary agent keeps no engine archive: it has no session tools
	// to read one with, so the engine must not point it at one either (the
	// archive note in the system message names those tools). Its conversation
	// window and summary are still kept by the session store.
	engineArchiveDir := archiveDir
	stateModes := agentModes(al.GetConfig(), agent.StateDir)
	if agent.Spec.Fresh {
		engineArchiveDir = ""
	}
	opts := append([]ctxengine.Option{
		ctxengine.WithContextWindow(al.sessionLimits(agent, sessionKey).contextWindow),
		ctxengine.WithArchiveDir(engineArchiveDir),
		ctxengine.WithFileModes(fsmode.Modes{Dir: stateModes.Dir, File: stateModes.File}),
		ctxengine.WithLogger(logger.SDK()),
		ctxengine.WithModelCaller(caller),
		ctxengine.WithCompressModel(ctxengine.ModelChain{Primary: effectiveCompressModel}),
		ctxengine.WithCompressionProfileDir(agent.StateDir),
		ctxengine.WithCompactDebug(debugCapture),
		ctxengine.WithFailureDump(failureDump),
		ctxengine.WithCompactionReporter(reporter),
		// A person whose reply waits on a compaction is told so.
		ctxengine.WithCompactionStartFunc(loop.CompactionStart),
		// The engine stops automatic compaction for a session after repeated
		// summarization failures; the context then grows until the safety-net
		// pass, so an operator should know.
		ctxengine.WithBreakerTrippedHook(func(sessionKey string, failures int) {
			al.Alerter().Send(alerter.Alert{
				Title:       "Context compaction breaker tripped",
				Description: fmt.Sprintf("%d consecutive automatic compactions failed for agent %s; normal compaction is suspended for this session and only the safety-net pass still runs", failures, agent.ID),
				EventID:     sessionKey,
			})
		}),
	}, agent.CompressOpts...)
	cm := ctxengine.New(sessionKey, agent.Sessions, opts...)

	// Issue a session token so session-scoped MCP tools can identify this session.
	// The loop renders it into the system prompt (sessionTokenLayer) on every
	// dispatch, so it lives on the entry.
	al.mu.RLock()
	sti := al.sessionTokenIssuer
	al.mu.RUnlock()
	token := ""
	if sti != nil && !agent.Spec.Fresh { // a fresh temporary agent has no tools to call with it
		token = sti.Issue(agent.ID, sessionKey, archiveDir)
	}

	// Cognitive-memory session — cognitive agents ONLY; nil for every other
	// agent, and every method on a nil session is a no-op.
	mem := al.wireCognitiveMemory(agent, sessionKey)

	newEntry := &cmEntry{
		cm:         cm,
		sessionKey: sessionKey,
		store:      agent.Sessions,
		mem:        mem,
	}
	newEntry.touch()
	newEntry.setToken(token)
	newEntry.refcount.Store(1)

	// We hold the build slot for bk, so no other builder can have stored this
	// key since takeCachedEntry found nothing to share; a plain Store clobbers
	// nobody.
	al.contextManagers.Store(bk.key, newEntry)
	return newEntry
}

// summaryModelName is how the finish line names a summarization model: its
// model_name.
func summaryModelName(c models.Candidate) string {
	return cmp.Or(c.Name, c.Model)
}
