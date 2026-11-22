// ClawEh
// License: MIT

package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tenebris-tech/alerter"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/cogmem"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/spawnllm/openai_compat"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/dump"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
)

// providerLLMClient is one model of the summarization chain;
// compressModelCaller walks a list of them. Each call is made on the model's
// single-model chain in the catalogue (begun anew for every call, so it
// follows a reload), which skips it while it is parked and records a failure
// against its health.
//
// Every request asks for a JSON-object response; the provider gates that on
// protocol capability (see openai_compat.Provider), and non-capable
// providers silently drop it and log DBG.
type providerLLMClient struct {
	catalog *models.Catalog
	// alias is the model's model_name, model its wire model id and
	// ourProvider its provider name, as the finish line logs them.
	alias       string
	model       string
	ourProvider string
	// workspace, when set, runs a CLI model isolated in it (a fresh
	// temporary agent's).
	workspace string
}

// Model returns the wire model this client dispatches to, used to label
// per-invocation entries in the compaction report.
func (c *providerLLMClient) Model() string { return c.model }

// chain begins the model's chain for one call.
func (c *providerLLMClient) chain() *models.Chain {
	ch := providers.ModelSet(c.catalog, c.alias).Begin(models.Chat)
	if c.workspace != "" {
		ch = ch.Isolated(c.workspace)
	}
	return ch
}

// compressModelCaller is the host's ctxengine.ModelCaller: it walks the
// agent's summarization chain in order (agent summarization_models → global
// summarization.models → the agent's primary model), skipping models the
// engine excluded and models the catalogue has parked, and returns the first
// reply together with the model that produced it. A transport error moves on
// to the next model and is recorded against the model's health, so an
// out-of-credits summarizer is not hammered on every compaction; the last
// error is returned when every model fails.
type compressModelCaller struct {
	clients []*providerLLMClient
	// clock times the cooldown a skipped model has left.
	clock clock.Clock
	// caller and sessionKey label the log lines.
	caller     llmCaller
	sessionKey string
	// id is who the calls are billed to: the agent whose conversation or
	// memory is being summarized.
	id global.Identity
	// noSessionKey leaves session_key off the finish line, for a caller whose
	// sessionKey names no conversation (memory consolidation).
	noSessionKey bool
}

// Complete implements ctxengine.ModelCaller.
func (c *compressModelCaller) Complete(ctx context.Context, req ctxengine.ModelRequest) (ctxengine.ModelReply, error) {
	content, finishReason, model, err := c.complete(ctx, req.System, req.User, req.JSONObject, req.Exclude)
	return ctxengine.ModelReply{Content: content, FinishReason: finishReason, Model: model}, err
}

// complete is the chain walk itself, in plain types so another caller shape
// (the cognitive-memory consolidation model) can wrap it without an import.
// It returns the reply and the model that served it; on failure the model is
// the last one tried (or "" when nothing was), so a report can still name it.
// When ctx ends the walk stops with ctx's error.
func (c *compressModelCaller) complete(ctx context.Context, system, user string, jsonObject bool, exclude []string) (content, finishReason, model string, err error) {
	messages := []providers.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	var opts map[string]any
	if jsonObject {
		opts = map[string]any{
			openai_compat.ResponseFormatJSONObjectOption: true,
		}
	}
	var lastErr error
	lastModel := ""
	excluded, cooling := 0, 0
	for _, cl := range c.clients {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", lastModel, ctxErr
		}
		if cl.model != "" && slices.Contains(exclude, cl.model) {
			excluded++
			continue
		}
		if info, ok := cl.catalog.Get(cl.alias); ok && !info.Health.Available {
			cooling++
			logger.InfoCF("llmcontext", "compression model in cooldown; skipping", map[string]any{
				"agent_id":  c.caller.agentID,
				"session":   c.sessionKey,
				"model":     cl.model,
				"remaining": max(clock.Or(c.clock).Until(info.Health.Until), 0).Round(time.Second).String(),
			})
			continue
		}
		sessionKey := c.sessionKey
		if c.noSessionKey {
			sessionKey = ""
		}
		var last finishRecorder
		res, chatErr := cl.chain().Chat(ctx, c.id, models.Request{Messages: messages, Options: opts},
			last.option(func(_ models.Candidate, elapsed time.Duration, resp *providers.LLMResponse, err error) {
				emitLLMFinishEvent(llmFinish{
					caller:         c.caller,
					kind:           llmKindCompression,
					ourProvider:    cl.ourProvider,
					ourModel:       cmp.Or(cl.alias, cl.model),
					requestedModel: cl.model,
					sessionKey:     sessionKey,
					turnID:         turnIDFrom(ctx),
					elapsedMs:      elapsed.Milliseconds(),
					resp:           resp,
					err:            err,
				})
			}))
		if chatErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", cl.model, ctxErr
			}
			if !last.called {
				// Parked between the check above and the call.
				cooling++
				continue
			}
			lastErr, lastModel = last.err, cl.model
			if lastErr == nil {
				lastErr = chatErr
			}
			logger.WarnCF("llmcontext", "compression model call failed; trying next in chain", map[string]any{
				"agent_id": c.caller.agentID,
				"session":  c.sessionKey,
				"model":    cl.model,
				"error":    redactedError(lastErr),
			})
			continue
		}
		return res.Response.Content, res.Response.FinishReason, cl.model, nil
	}
	if lastErr != nil {
		return "", "", lastModel, lastErr
	}
	return "", "", "", fmt.Errorf("%w: %d excluded, %d in cooldown", ctxengine.ErrNoModel, excluded, cooling)
}

// resolveCompressModelTarget resolves a configured compress_model reference into
// the (alias, protocol, modelID) triple that can be handed to the provider
// dispatcher. Bare aliases and shorthand model IDs are looked up against the
// loaded models, mirroring resolveFromModelList in instance.go.
//
// The returned alias is the resolved entry's model_name; the dispatcher uses
// it as the cache/lookup key so per-entry openai_compat state
// (response_log_file, reasoning_effort, extra_body, …) is honoured when
// multiple entries share the same wire model.
//
// Returns ("", "", "", false) when the reference cannot be resolved against the
// configured models — callers should then fall back to the agent's default
// provider rather than guess a protocol.
// resolveCompressModelChain returns the ordered, de-duplicated summarization
// model chain for an agent: its own summarization_models first, then the global
// summarization.models. Blank entries are skipped; the first occurrence of each
// model name wins. The agent's primary model is appended separately by the
// caller as a final fallback.
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

// resolveCompressClient resolves one summarization model to a client for
// the chain walker, or nil when it names no enabled model.
func (al *AgentLoop) resolveCompressClient(agent *AgentInstance, compressModelName, sessionKey string) *providerLLMClient {
	cfg := al.GetConfig()
	alias, modelID, ok := resolveCompressModelTarget(cfg, compressModelName)
	if !ok {
		logger.WarnCF("llmcontext", "compression model not found in enabled models; skipping", map[string]any{
			"agent_id":  agent.ID,
			"requested": compressModelName,
			"session":   sessionKey,
		})
		return nil
	}
	logger.DebugCF("llmcontext", "compression model resolved", map[string]any{
		"agent_id":  agent.ID,
		"requested": compressModelName,
		"alias":     alias,
		"model":     modelID,
		"session":   sessionKey,
	})
	return al.compressClient(agent, cfg, alias, modelID)
}

// compressClient is the summarization client of model alias for agent: a
// fresh temporary agent's CLI model runs isolated in its workspace.
func (al *AgentLoop) compressClient(agent *AgentInstance, cfg *config.Config, alias, modelID string) *providerLLMClient {
	cl := &providerLLMClient{catalog: al.catalog, alias: alias, model: modelID, ourProvider: compressProviderName(cfg, alias)}
	if agent.Spec.Fresh {
		cl.workspace = agent.Workspace
	}
	return cl
}

// resolveDefaultCompressClient resolves the agent's primary model to a
// client, the chain walker's last resort; nil when it names no enabled model.
func (al *AgentLoop) resolveDefaultCompressClient(agent *AgentInstance, sessionKey string) *providerLLMClient {
	cfg := al.GetConfig()
	primary := strings.TrimSpace(agent.Model)
	alias, modelID, ok := resolveCompressModelTarget(cfg, primary)
	if primary == "" || !ok {
		return nil
	}
	logger.DebugCF("llmcontext", "compression: agent primary appended as final fallback", map[string]any{
		"agent_id": agent.ID,
		"alias":    alias,
		"model":    modelID,
		"session":  sessionKey,
	})
	return al.compressClient(agent, cfg, alias, modelID)
}

// newCompressModelCaller builds the summarization ModelCaller for an agent and
// session: the agent's own summarization_models first, then the global
// cfg.Summarization.Models, then the agent's primary model as a last-resort
// fallback so summarization still works when the lists are empty or every
// configured model fails. Cooldowns are shared with the main fallback chain
// so a model parked by either path (e.g. an out-of-credits 402) is skipped by
// both. Also returns the effective (first) model name for the summary stamp.
func (al *AgentLoop) newCompressModelCaller(agent *AgentInstance, sessionKey string) (*compressModelCaller, string) {
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

	effective := ""
	chainNames := resolveCompressModelChain(agentModels, globalModels)
	clients := make([]*providerLLMClient, 0, len(chainNames)+1)
	for _, name := range chainNames {
		if effective == "" {
			effective = name
		}
		if cl := al.resolveCompressClient(agent, name, sessionKey); cl != nil {
			clients = append(clients, cl)
		}
	}
	if cl := al.resolveDefaultCompressClient(agent, sessionKey); cl != nil {
		clients = append(clients, cl)
	}

	// One clear line (in claw.log) showing the whole compression chain that will
	// be tried in order — the per-client detail above goes to the structured log
	// too, but this is the at-a-glance summary of what's actually in effect.
	logger.InfoCF("llmcontext", "compression model chain", map[string]any{
		"agent_id": agent.ID,
		"session":  sessionKey,
		"chain":    append(append([]string{}, chainNames...), agent.Model+" (agent default)"),
	})

	// The rendered "Generated: <time> by <model>" line names the effective
	// compress model; it falls back to the agent's primary model when no
	// summarization models are configured — that is the model the appended
	// default client actually runs against.
	if effective == "" {
		effective = strings.TrimSpace(agent.Model)
	}
	return &compressModelCaller{
		clients:    clients,
		clock:      registryClock{al},
		caller:     callerOf(agent),
		sessionKey: sessionKey,
		id:         agent.Identity(),
	}, effective
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
	// resolveCompressClient / resolveDefaultCompressClient for per-entry rules.
	caller, effectiveCompressModel := al.newCompressModelCaller(agent, sessionKey)

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
		ctxengine.WithCompactionStartFunc(al.compactionStarted),
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
