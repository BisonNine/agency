// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	sdkdiscovery "go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/agentreg"
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/config"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
	toolsmsg "go.bison9.com/agency/tools/msg"
)

// agentBuilder returns the registry's BuildFunc: it builds the instance a spec
// describes and registers its tools, so every agent — config agents at start
// and on reload, temporary agents when they are created or rebuilt — gets its
// tools exactly once, from one place. cfg is the one being built against (on
// reload, al.cfg is still the old one at this point). A fresh temporary agent
// gets no tools at all.
//
// The agent's models are assigned as assignBuilt says; reload is the
// reload the builder is made for, nil for the registry's first one.
func (al *AgentLoop) agentBuilder(reload *reloadAssignments) agentreg.BuildFunc[*AgentInstance] {
	return func(ctx context.Context, cfg *config.Config, spec agentreg.Spec) (*AgentInstance, error) {
		inst, err := newAgentInstance(spec, &cfg.Agents.Defaults, cfg)
		if err != nil {
			return nil, err
		}
		al.assignBuilt(reload, cfg, inst)
		if inst.HumanModel != "" {
			// A human agent runs no model: no vision side-model, no tools.
			return inst, nil
		}
		// Wire the vision-describe side-model chain onto the instance (no-op when
		// no vision model is configured).
		al.wireVisionClients(cfg, inst)
		if spec.Fresh {
			return inst, nil
		}
		al.registerAgentTools(ctx, inst, cfg)
		if inst.IsTemp() {
			// A config agent gets these after the registry is built (the gateway
			// registers its tools); an agent created later gets them now. Its
			// MCP tools come once it is registered (agentInserted).
			al.registerExtraTools(inst)
		}
		return inst, nil
	}
}

// agentInserted is the registry's InsertedFunc: a temporary agent CreateClone or CreateFresh has
// just made visible gets the current MCP tool set. Done after insertion and
// under the MCP refresh lock, so a server that is replaced while the agent is
// being created is either seen here or re-registered onto it by the refresh.
func (al *AgentLoop) agentInserted(_ agentreg.Spec, inst *AgentInstance) {
	if inst.toolless() {
		return
	}
	al.mcp.refreshMu.Lock()
	defer al.mcp.refreshMu.Unlock()
	al.registerMCPToolsOn(inst)
}

// registerAgentTools builds the full ToolDeps for one agent (session closures,
// the sub-agent spawner, the message tool, the agent's models) and registers
// every allowed provider tool on it. A clone's tools act as its source (see
// toolIdentity) but are bound to the clone's own state directory and session.
func (al *AgentLoop) registerAgentTools(
	ctx context.Context,
	currentAgent *AgentInstance,
	cfg *config.Config,
) {
	agentID := currentAgent.toolIdentity()
	currentAgentID := currentAgent.ID
	messageTool := al.newMessageTool()
	candidateResolver := al.spawnCandidates
	spawnAllowlist := func(callerID, targetID string) bool {
		return canSpawnSubagent(al.GetRegistry(), currentAgentID, targetID)
	}
	spawner := al.newSpawner(currentAgent, agentID, cfg, candidateResolver, spawnAllowlist)

	deps := tools.ToolDeps{
		Cfg:             cfg,
		AgentCfg:        currentAgent.Config,
		AgentID:         agentID,
		Workspace:       currentAgent.Workspace,
		StateDir:        currentAgent.StateDir,
		EphemeralMemory: currentAgent.Spec.Ephemeral,
		TempAgent:       currentAgent.IsTemp(),
		TempPurpose:     currentAgent.Spec.Purpose,
		CloneMarker:     cloneMarker(currentAgent),
		Models:          al.catalog.Set(currentAgent.ID),
		SpawnAllowlist:  spawnAllowlist,
		Agents:          newAgentServices(al, currentAgentID),
		Messenger:       al,
		Files:           al,
		Spawn:           spawner,
		CompactFn:       al.compactFn(currentAgent),
		SessionInfoFn:   al.sessionInfoFn(currentAgent),
		ClearFn:         al.clearFn(currentAgent),
		MessageTool:     messageTool,
	}

	// Progressive discovery is a single global switch (default off). When on,
	// native tools and the cogmem suite stay always-on; the fusion/maestro suites
	// (and MCP tools, in loop_mcp) are hidden behind search_tools.
	discovery := cfg.Tools.Discovery.Enabled
	currentAgent.DiscoveryActive = discovery
	currentAgent.AlwaysShownNamespaces = discoveryPins(cfg, currentAgent.Config)
	if currentAgent.ContextBuilder != nil {
		currentAgent.ContextBuilder.WithToolDiscovery(discovery)
	}
	al.registerProviderTools(currentAgent, cfg, deps, discovery)
	if discovery {
		al.registerDiscoveryMetaTools(ctx, currentAgent, cfg)
	}
}

// newMessageTool builds the msg_send tool. Its send waits one publishTimeout
// for queueing the message and learning its delivery: a channel reports an
// offline, unknown or stopped recipient at once (never retried); a slower
// outcome is reported as queued.
func (al *AgentLoop) newMessageTool() tools.Tool {
	mt := toolsmsg.NewMessageTool()
	mt.SetSendCallback(func(ctx context.Context, channel, chatID, content string) error {
		pubCtx, pubCancel := context.WithTimeout(ctx, publishTimeout)
		defer pubCancel()
		// The tool sends only to a chat (never an internal channel), whose
		// channel reports the delivery.
		delivered := make(chan error, 1)
		msg := bus.OutboundMessage{Channel: channel, ChatID: chatID, Content: content, OnDelivery: func(err error) {
			select {
			case delivered <- err:
			default:
			}
		}}
		if err := al.bus.PublishOutbound(pubCtx, msg); err != nil {
			return err
		}
		select {
		case err := <-delivered:
			return err
		case <-pubCtx.Done():
			return toolsmsg.ErrQueued
		}
	})
	return mt
}

// spawnCandidates is the spawn tool's candidate resolver: the worker
// candidates of the target agent, possibly none. The registry is looked up at
// call time, because tools are registered while the registry is still being
// built.
func (al *AgentLoop) spawnCandidates(targetAgentID string) ([]providers.FallbackCandidate, bool) {
	target, ok := al.GetRegistry().Get(targetAgentID)
	if !ok {
		return nil, false
	}
	return al.workerCandidates(target), true
}

// hasWorkerModels reports whether the agent has configured worker models
// (subagents.models), whether or not any of them is available.
func (al *AgentLoop) hasWorkerModels(agentID string) bool {
	target, ok := al.GetRegistry().Get(agentID)
	return ok && target.Subagents != nil && len(target.Subagents.Models) > 0
}

// newSpawner builds the agent's sub-agent launcher, shared by the internal
// spawn tool and any external/MCP tool through Deps.Spawn. A clone's tasks
// are its source's (same tasks directory, same owner, agentID), so a
// background task it starts reports to the source's main conversation, like
// every other late result of a clone (see asyncResultTarget).
func (al *AgentLoop) newSpawner(
	currentAgent *AgentInstance,
	agentID string,
	cfg *config.Config,
	candidateResolver func(string) ([]providers.FallbackCandidate, bool),
	spawnAllowlist func(callerID, targetID string) bool,
) *toolsagents.Spawner {
	spawnMgr := toolsagents.NewSubagentManager(toolsagents.SubagentManagerConfig{
		Workspace:         currentAgent.Workspace,
		Live:              al.taskLive,
		SelfResolver:      func() []providers.FallbackCandidate { return al.workerCandidates(currentAgent) },
		CallerAgentID:     agentID,
		CandidateResolver: candidateResolver,
		WorkerModelsSet:   al.hasWorkerModels,
		RunFull:           al.runSubagentTask,
		Alerter:           al.Alerter(),
		TaskRunner:        &taskModelHost{al: al, clone: cloneMarker(currentAgent), id: currentAgent.Identity()},
	})
	currentAgent.spawnMgr = spawnMgr
	spawner := toolsagents.NewSpawner(spawnMgr)
	spawner.SetMaxDepth(cfg.Agents.Defaults.GetMaxSubagentDepth())
	currentAgentID := currentAgent.ID
	spawner.SetAllowlistChecker(func(targetID string) bool {
		return spawnAllowlist(currentAgentID, targetID)
	})
	spawner.SetCardChecker(func(id string) bool { return cardExists(al.GetConfig(), id) })
	return spawner
}

// compactFn is session_compact's closure for agent: it returns the
// compaction report and the resulting rendered summary alongside the error.
func (al *AgentLoop) compactFn(agent *AgentInstance) func(ctx context.Context, sessionKey string) (string, string, error) {
	return func(ctx context.Context, sessionKey string) (string, string, error) {
		ctx = providers.WithAgentID(ctx, agent.ID)
		cm, release := al.getContextManager(agent, sessionKey) //nolint:contextcheck // compaction reporter: ctxengine's callback has no context, so it publishes on its own
		defer release()
		err := cm.Compact(ctx)
		report := ""
		if r := cm.LastCompactionReport(); r != nil {
			report = r.String()
		}
		return report, cm.RenderedSummary(), err
	}
}

// clearFn is session_clear's closure for agent. It is rate-limited and
// publishes a reset-tagged self-handoff inbound that resets the session and
// restarts the turn at a clean boundary: history is never wiped mid-turn.
func (al *AgentLoop) clearFn(agent *AgentInstance) func(ctx context.Context, sessionKey, message string) error {
	return func(ctx context.Context, sessionKey, message string) error {
		if !al.allowSelfClear(sessionKey) {
			return errors.New("session_clear is rate-limited; wait a few seconds before clearing again")
		}
		meta := map[string]string{
			metaSessionReset:              "true",
			metadataKeyPreresolvedAgentID: agent.ID,
		}
		inbound := bus.InboundMessage{
			Channel:    tools.ToolChannel(ctx),
			ChatID:     tools.ToolChatID(ctx),
			SenderID:   "system",
			Internal:   true,
			SessionKey: sessionKey,
			Content:    wrapClearNotice(message),
			Metadata:   meta,
		}
		if inbound.ChatID != "" && inbound.ChatID != "direct" {
			inbound.Peer = bus.Peer{Kind: "channel", ID: inbound.ChatID}
		}
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		defer cancel()
		return al.bus.PublishInbound(pubCtx, inbound)
	}
}

// sessionInfoFn is session_info's closure for agent.
func (al *AgentLoop) sessionInfoFn(agent *AgentInstance) func(ctx context.Context, sessionKey string) (*tools.SessionInfo, error) {
	return func(ctx context.Context, sessionKey string) (*tools.SessionInfo, error) {
		return buildSessionInfo(al, agent, sessionKey) //nolint:contextcheck // compaction reporter: ctxengine's callback has no context, so it publishes on its own
	}
}

// registerProviderTools registers on agent every tool the available
// providers build from deps: a native tool subject to the agent's allowlist,
// a suite tool gated by its suite's switch (never by the allowlist, always by
// the deny list), and hidden behind discovery unless pinned.
func (al *AgentLoop) registerProviderTools(currentAgent *AgentInstance, cfg *config.Config, deps tools.ToolDeps, discovery bool) {
	agentCfg := currentAgent.Config
	for _, p := range tools.GetProviders() {
		if ok, _ := p.Available(cfg); !ok {
			continue
		}
		suite := ""
		if sp, ok := p.(tools.SuiteProvider); ok {
			suite = sp.Suite()
		}
		for _, t := range p.Build(deps) {
			switch {
			case suite == "":
				// Native: always visible, subject to the per-tool allowlist.
				if agentCfg.IsToolAllowed(t.Name()) {
					currentAgent.Tools.Register(t)
				}
			case agentCfg.IsToolDenied(t.Name()):
				// Suite tools skip the allow list (gated as a unit by their
				// toggle) but never the agent's deny list. Mirrors the
				// execution-time check in tools.ToolRegistry.ExecuteWithContext.
				logger.DebugCF("agent", "Skipping suite tool registration: denied by agent deny_tools",
					map[string]any{"agent_id": deps.AgentID, "tool": t.Name(), "suite": suite})
			case suite == suiteCogmem:
				currentAgent.Tools.RegisterSuite(t) // cogmem: always-on, never hidden
			case discovery:
				// fusion/maestro behind discovery unless a namespace pinned via
				// always_shown_namespaces keeps it listed; HideEntries carries
				// any reveal-together group (e.g. a fusion service) so
				// get_tool_details loads the whole set.
				e := sdkdiscovery.HideEntries([]toolkit.Entry{{Tool: t}}, alwaysShown(currentAgent.AlwaysShownNamespaces))[0]
				currentAgent.Tools.RegisterEntries(tools.ToolEntry{Entry: e, SuiteExempt: true})
			default:
				// Non-discovery suites.
				currentAgent.Tools.RegisterSuite(t)
			}
		}
	}
}

// retireAgent is the registry's RetireFunc: before a temporary agent is
// closed and its directory removed, its one session's context manager is
// closed (refused while something holds it), its session tokens revoked and
// the per-session caches the loop keeps for it dropped. A config agent's
// instance is retired only when a reload replaced it; the agent lives on in
// its new instance, so only what still uses the old one is released.
func (al *AgentLoop) retireAgent(spec agentreg.Spec, inst *AgentInstance) error {
	if spec.Origin == agentreg.OriginConfig {
		return al.releaseReplacedInstance(inst)
	}
	sessionKey := routing.BuildAgentMainSessionKey(inst.ID)
	if !al.dropContextManager(context.Background(), inst, sessionKey, evictReasonTempDeleted) {
		return fmt.Errorf("session %s is still in use", sessionKey)
	}
	al.releaseSessionPins(sessionKey)
	al.mu.RLock()
	sti := al.sessionTokenIssuer
	al.mu.RUnlock()
	if sti != nil {
		sti.RevokeAgent(inst.ID)
	}
	al.forgetSessionCaches(inst, sessionKey)
	// Whispers and the agent's models wait for the agent, not the instance:
	// a reload's rebuild keeps them (the agent is still registered, on its
	// new instance); a deletion drops them.
	if _, ok := al.GetRegistry().Get(inst.ID); !ok {
		al.whispers.drop(inst.ID)
		al.catalog.RemoveSet(inst.ID)
	}
	return nil
}

// registerExtraTools registers on agent the tools added with RegisterTool
// (the gateway's cron tool), honouring its allowlist.
func (al *AgentLoop) registerExtraTools(agent *AgentInstance) {
	if agent.toolless() {
		return
	}
	al.extraToolsMu.Lock()
	extra := slices.Clone(al.extraTools)
	al.extraToolsMu.Unlock()
	for _, t := range extra {
		if agent.Config.IsToolAllowed(t.Name()) {
			agent.Tools.Register(t)
		}
	}
}

// suiteCogmem is the one suite that is never subject to progressive discovery —
// cognitive memory is fundamental to how the agent works, so it stays always-on.
const suiteCogmem = "cogmem"

// alwaysShown keeps a discovery-eligible tool (a fusion/maestro suite tool or
// an upstream MCP tool) listed when its internal name matches one of pins
// (always_shown_namespaces and the Maestro entry tool) under MatchVisibility,
// which strips the mcp_ prefix and matches a namespace prefix.
func alwaysShown(pins []string) sdkdiscovery.HideOption {
	return sdkdiscovery.AlwaysShownFunc(func(name string) bool { return config.MatchVisibility(pins, name) })
}

// discoveryPins returns the visibility pins for an agent's in-loop tool list:
// the configured always_shown_namespaces plus, when the agent has Maestro and
// discovery is on, Maestro's entry-point tool, so the model can always reach the
// orientation guide and search for the rest. MatchVisibility matches by prefix,
// so a full tool name pins exactly that tool. This affects only the in-loop
// model; the MCP host always serves the full tool list.
func discoveryPins(cfg *config.Config, agentCfg *config.AgentConfig) []string {
	pins := append([]string(nil), cfg.AlwaysShownNamespaces()...)
	if cfg.Tools.Discovery.Enabled && agentCfg.MaestroEnabled() {
		pins = append(pins, global.MaestroEntryTool)
	}
	return pins
}

// registerDiscoveryMetaTools registers the search_tools / get_tool_details entry
// points. They are suite-exempt (gated by the discovery decision, not the per-tool
// allowlist) and present only when discovery is active for the agent.
func (al *AgentLoop) registerDiscoveryMetaTools(ctx context.Context, agent *AgentInstance, cfg *config.Config) {
	maxHits := cfg.Tools.Discovery.MaxSearchResults
	if maxHits <= 0 {
		maxHits = config.DefaultDiscoveryMaxSearchHits
	}
	// Suite-exempt from the allow list, but the agent's deny list still applies.
	agent.Tools.RegisterDiscoveryTools(ctx, maxHits, agent.Config.IsToolDenied)
}

// RegisterTool registers a host-built tool (the gateway's cron tool) on every
// agent that allows it, and remembers it for the temporary agents created
// later. A fresh temporary agent gets no tools.
func (al *AgentLoop) RegisterTool(tool tools.Tool) {
	al.extraToolsMu.Lock()
	al.extraTools = slices.DeleteFunc(al.extraTools, func(t tools.Tool) bool { return t.Name() == tool.Name() })
	al.extraTools = append(al.extraTools, tool)
	al.extraToolsMu.Unlock()

	registry := al.GetRegistry()
	for _, agentID := range registry.All() {
		agent, ok := registry.Get(agentID)
		if !ok || agent.toolless() {
			continue
		}
		// Per-agent allowlist check: skip registration if the agent config
		// explicitly denies this tool. Config is always non-nil after construction.
		if !agent.Config.IsToolAllowed(tool.Name()) {
			logger.DebugCF("agent", "Skipping tool registration: not allowed by agent config",
				map[string]any{
					"agent_id": agentID,
					"tool":     tool.Name(),
				})
			continue
		}
		agent.Tools.Register(tool)
	}
}

// stopTyping clears the typing indicator for a channel/chatID when a turn ends
// without sending a reply (the send path stops typing on its own). No-op when
// the channel manager is unset or the target is empty.
func (al *AgentLoop) stopTyping(channel, chatID string) {
	if al.channelManager != nil && channel != "" && chatID != "" {
		al.channelManager.StopTyping(channel, chatID)
	}
}

// startProgressUpdates edits the turn's "Thinking…" placeholder every interval
// so a long-running turn (many tool calls, slow model) never looks dead. The
// returned function stops the updater and must be deferred. It is a no-op (and
// returns a no-op stopper) for non-user turns or when progress is disabled.
// completed is the live count of finished tool calls, shared with the LLM loop.
func (al *AgentLoop) startProgressUpdates(
	ctx context.Context, channel, chatID string, interval time.Duration, completed *atomic.Int64,
) func() {
	if interval <= 0 || al.channelManager == nil || channel == "" || channel == "system" || chatID == "" {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				n := completed.Load()
				var text string
				if n == 0 {
					text = "⏳ Still working…"
				} else {
					text = fmt.Sprintf("⏳ Still working… %d tool call(s) completed so far.", n)
				}
				editCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				al.channelManager.UpdatePlaceholder(editCtx, channel, chatID, text)
				cancel()
			}
		}
	}()
	// Block until the updater has fully exited so no placeholder edit can land
	// after the turn's final reply (which consumes the placeholder).
	return func() {
		close(stop)
		<-done
	}
}

// superviseTasks periodically relaunches interrupted callback tasks across every
// agent's workspace. It exits on ctx cancellation or Close().
func (al *AgentLoop) superviseTasks(ctx context.Context) {
	ticker := time.NewTicker(global.TaskSupervisorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-al.superStop:
			return
		case <-ticker.C:
			al.runTaskSupervision() //nolint:contextcheck // relaunched callback tasks run detached from the supervisor, like fresh spawns
		}
	}
}

// runTaskSupervision runs one supervision pass over the config agents' spawn
// managers. A clone shares its source's tasks directory, so the source's
// manager covers the tasks a clone started.
func (al *AgentLoop) runTaskSupervision() {
	registry := al.GetRegistry()
	var managers []*toolsagents.SubagentManager
	for _, id := range registry.List() {
		if a, ok := registry.Get(id); ok && a.spawnMgr != nil {
			managers = append(managers, a.spawnMgr)
		}
	}

	now := time.Now().Unix()
	for _, m := range managers {
		m.SuperviseOnce(now, func(rec *toolsagents.TaskRecord) tools.AsyncCallback {
			return al.taskPointerCallback(rec.Channel, rec.ChatID, rec.OwnerAgentID, rec.SpawnDepth)
		})
	}
}

// taskPointerCallback builds the completion callback for a relaunched task: it
// publishes the compact completion pointer to the task's origin channel (the
// agent reads the referenced result file). Mirrors the inline async-tool callback
// used for the initial in-turn spawn. spawnDepth is the spawning turn's depth,
// carried on the re-injected message so the re-entered turn is not lower.
func (al *AgentLoop) taskPointerCallback(channel, chatID, ownerAgentID string, spawnDepth int) tools.AsyncCallback {
	return func(cbCtx context.Context, result *tools.ToolResult) {
		if result == nil {
			return
		}
		// A task started by an asked turn has no chat for user-facing output.
		if !result.Silent && result.ForUser != "" && channel != constants.AgentMessageChannel {
			outCtx, outCancel := context.WithTimeout(context.WithoutCancel(cbCtx), publishTimeout)
			if err := al.bus.PublishOutbound(outCtx, bus.OutboundMessage{
				Channel: channel,
				ChatID:  chatID,
				Content: result.ForUser,
			}); err != nil {
				logger.WarnCF("agent", "Failed to publish async task result to user", map[string]any{
					"channel": channel,
					"error":   err.Error(),
				})
			}
			outCancel()
		}
		content := result.ForLLM
		if content == "" && result.Err != nil {
			content = result.Err.Error()
		}
		if content == "" {
			return
		}
		pubCtx, pubCancel := context.WithTimeout(context.WithoutCancel(cbCtx), publishTimeout)
		msg := bus.InboundMessage{
			Channel:  "system",
			SenderID: "async:agent_spawn",
			Internal: true,
			ChatID:   chatID,
			Content:  content,
			Metadata: map[string]string{bus.MetaOriginChannel: channel},
		}
		if ownerAgentID != "" {
			msg.Metadata[metadataKeyPreresolvedAgentID] = ownerAgentID
			msg.SessionKey = routing.BuildAgentMainSessionKey(ownerAgentID)
		}
		msg.Metadata = bus.SetSpawnDepth(msg.Metadata, spawnDepth)
		if err := al.bus.PublishInbound(pubCtx, msg); err != nil {
			logger.WarnCF("agent", "Failed to publish async task result to agent", map[string]any{
				"channel": channel,
				"error":   err.Error(),
			})
		}
		pubCancel()
	}
}

// releaseReplacedInstance evicts every cached context manager still built on
// a replaced config instance's session store, so the store can be closed. It
// refuses while one is in use; the registry retries later. Managers already
// rebuilt on the new instance are left alone, and so are the agent's tokens
// and per-session caches, which belong to the agent, not the instance.
func (al *AgentLoop) releaseReplacedInstance(inst *AgentInstance) error {
	if inst.Sessions == nil {
		return nil
	}
	var busy string
	al.contextManagers.Range(func(key, value any) bool {
		entry, ok := value.(*cmEntry)
		if !ok || entry.store != inst.Sessions {
			return true
		}
		if !al.tryEvictEntry(context.Background(), fmt.Sprint(key), entry, evictReasonReload) {
			busy = entry.sessionKey
			return false
		}
		return true
	})
	if busy != "" {
		return fmt.Errorf("session %s is still in use", busy)
	}
	return nil
}
