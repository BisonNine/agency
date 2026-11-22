// ClawEh
// License: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/tenebris-tech/alerter"
	cogmemstore "go.bison9.com/agent/cogmem/store"
	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/models"

	"go.bison9.com/agency/cogmemhost"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/utils"
)

// compactionStateStore is the subset of the session store used to persist the
// per-session active model index inside the CompactionState record.
type compactionStateStore interface {
	GetCompactionState(sessionKey string) (memory.CompactionState, error)
	SetCompactionState(sessionKey string, state memory.CompactionState) error
}

// activeModelCacheKey builds the cache key for a session's active model index.
func activeModelCacheKey(agentID, sessionKey string) string {
	return agentID + "\x00" + sessionKey
}

// forgetSessionCaches drops the per-session state the loop holds in memory
// for a session (its model selection and what serves it, reasoning, tool
// activity, session_clear rate limit), for a temporary agent that is being
// deleted.
func (al *AgentLoop) forgetSessionCaches(agent *AgentInstance, sessionKey string) {
	key := activeModelCacheKey(agent.ID, sessionKey)
	if al.catalog != nil {
		al.catalog.Set(agent.ID).Forget(models.Scope(sessionKey))
	}
	al.exposeReasoningMu.Lock()
	delete(al.exposeReasoningCache, key)
	al.exposeReasoningMu.Unlock()
	al.showToolActivityMu.Lock()
	delete(al.showToolActivityCache, key)
	al.showToolActivityMu.Unlock()
	al.lastSelfClear.Delete(sessionKey)
}

// chatModelNames are the agent's chat models in their order, as its set
// holds them: what /model lists and indexes.
func (al *AgentLoop) chatModelNames(agent *AgentInstance) []string {
	list := al.catalog.Set(agent.ID).List(models.Chat)
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.Name)
	}
	return names
}

// activeModelName is the model the session's turns start on: the one /model
// selected, else the agent's first chat model; "" when it has no chat model
// (its turns fail with errNoModelConfigured).
func (al *AgentLoop) activeModelName(agent *AgentInstance, sessionKey string) string {
	set := al.catalog.Set(agent.ID)
	if name := set.Selected(models.Chat, models.Scope(sessionKey)).Name; name != "" {
		return name
	}
	if list := set.List(models.Chat); len(list) > 0 {
		return list[0].Name
	}
	return ""
}

// getActiveModelIndex returns the position of the session's active model in
// the agent's chat models (0 when none is selected).
func (al *AgentLoop) getActiveModelIndex(agent *AgentInstance, sessionKey string) int {
	name := al.catalog.Set(agent.ID).Selected(models.Chat, models.Scope(sessionKey)).Name
	return max(slices.Index(al.chatModelNames(agent), name), 0)
}

// sessionLimits are the limits of the model the session's turns run on: the
// one /model selected, else the agent's first.
func (al *AgentLoop) sessionLimits(agent *AgentInstance, sessionKey string) runLimits {
	return agent.limitsFor(al.activeModelName(agent, sessionKey))
}

// setActiveModelIndex makes the agent's idx-th chat model the session's
// active one. The set stores the choice through sessionSelections, so it
// survives a restart.
func (al *AgentLoop) setActiveModelIndex(agent *AgentInstance, sessionKey string, idx int) error {
	al.listsGate.RLock()
	defer al.listsGate.RUnlock()
	names := al.chatModelNames(agent)
	n := len(names)
	if n == 0 {
		return errors.New("this agent has no selectable models")
	}
	if idx < 0 || idx >= n {
		return fmt.Errorf("model index out of range (0-%d)", n-1)
	}
	return al.catalog.Set(agent.ID).Select(models.Chat, names[idx], models.Scope(sessionKey))
}

// sessionSelections is the catalogue's models.SelectionStore: a session's
// chat-model selection is kept as its active model index in the session's
// CompactionState, as /model has always stored it. Other purposes and the
// sets' default selections are not stored.
type sessionSelections struct{ al *AgentLoop }

// selectionAgent is the agent whose set is set, with its session store.
// It reads selectionAgents, never the registry: it runs under listsGate.
func (s sessionSelections) selectionAgent(set string) (*AgentInstance, compactionStateStore, bool) {
	v, ok := s.al.selectionAgents.Load(set)
	if !ok {
		return nil, nil, false
	}
	inst, ok := v.(*AgentInstance)
	if !ok || inst == nil {
		return nil, nil, false
	}
	store, ok := inst.Sessions.(compactionStateStore)
	return inst, store, ok
}

// LoadSelection reads the session's active model index and names the model
// at it, clamped to the agent's chat models; index 0 is no selection.
func (s sessionSelections) LoadSelection(set string, p models.Purpose, scope models.Scope) (string, error) {
	if p != models.Chat || scope == "" {
		return "", nil
	}
	inst, store, ok := s.selectionAgent(set)
	if !ok {
		// Not read yet, not "none": the catalogue reads it again next time.
		return "", fmt.Errorf("agent %q is not running yet", set)
	}
	st, err := store.GetCompactionState(string(scope))
	if err != nil {
		return "", nil //nolint:nilerr // an unreadable state means no saved choice, as before
	}
	names := s.al.chatModelNames(inst)
	idx := min(max(st.ActiveModelIndex, 0), len(names)-1)
	if idx <= 0 {
		return "", nil
	}
	return names[idx], nil
}

// SaveSelection stores the position of name among the agent's chat models
// as the session's active model index (0 for no selection). A store error is
// logged and alerted here, as before; the catalogue keeps the choice in
// memory either way.
func (s sessionSelections) SaveSelection(set string, p models.Purpose, scope models.Scope, name string) error {
	if p != models.Chat || scope == "" {
		return nil
	}
	inst, store, ok := s.selectionAgent(set)
	if !ok {
		return nil
	}
	sessionKey := string(scope)
	idx := max(slices.Index(s.al.chatModelNames(inst), name), 0)
	st, err := store.GetCompactionState(sessionKey)
	if err != nil {
		logger.WarnCF("agent", "active model index: load compaction state failed",
			map[string]any{"session_key": sessionKey, "error": err.Error()})
		return nil //nolint:nilerr // logged here, as before; the catalogue keeps the choice in memory
	}
	st.ActiveModelIndex = idx
	if err := store.SetCompactionState(sessionKey, st); err != nil {
		logger.WarnCF("agent", "active model index: persist failed",
			map[string]any{"session_key": sessionKey, "error": err.Error()})
		s.al.alertSessionStateNotPersisted(inst, "active model index", err)
	} else {
		s.al.storeFaultOver(inst, alertSessionStateNotStored)
	}
	return nil
}

// getExposeReasoning returns whether the session exposes the model's reasoning
// to the user, loading it from the session store on a cache miss. Default false.
func (al *AgentLoop) getExposeReasoning(agent *AgentInstance, sessionKey string) bool {
	key := activeModelCacheKey(agent.ID, sessionKey)

	al.exposeReasoningMu.Lock()
	if v, ok := al.exposeReasoningCache[key]; ok {
		al.exposeReasoningMu.Unlock()
		return v
	}
	al.exposeReasoningMu.Unlock()

	v := false
	if store, ok := agent.Sessions.(compactionStateStore); ok {
		if st, err := store.GetCompactionState(sessionKey); err == nil {
			v = st.ExposeReasoning
		}
	}

	al.exposeReasoningMu.Lock()
	al.exposeReasoningCache[key] = v
	al.exposeReasoningMu.Unlock()
	return v
}

// setExposeReasoning sets the session's expose-reasoning flag, updating the cache
// and persisting it through CompactionState (best-effort).
func (al *AgentLoop) setExposeReasoning(agent *AgentInstance, sessionKey string, v bool) {
	key := activeModelCacheKey(agent.ID, sessionKey)
	al.exposeReasoningMu.Lock()
	al.exposeReasoningCache[key] = v
	al.exposeReasoningMu.Unlock()

	if store, ok := agent.Sessions.(compactionStateStore); ok {
		st, err := store.GetCompactionState(sessionKey)
		if err != nil {
			logger.WarnCF("agent", "expose reasoning: load compaction state failed",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
			return
		}
		st.ExposeReasoning = v
		if err := store.SetCompactionState(sessionKey, st); err != nil {
			logger.WarnCF("agent", "expose reasoning: persist failed",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
			al.alertSessionStateNotPersisted(agent, "expose reasoning", err)
		} else {
			al.storeFaultOver(agent, alertSessionStateNotStored)
		}
	}
}

// ToolActivityLine renders the one-line "/tools on" breadcrumb for a tool call
// dispatched over the MCP path (a CLI provider hitting claw's MCP server), or ""
// when the session has tool activity off, the agent is unknown, or the tool
// produces no summary. Wired into the MCP server as a ToolActivityNotifier so
// CLI-routed tool calls surface to the user the same way loop-dispatched ones do.
func (al *AgentLoop) ToolActivityLine(agentID, sessionKey, toolName string, args map[string]any) string {
	agent, ok := al.registry.Get(agentID)
	if !ok || agent == nil {
		return ""
	}
	if !al.getShowToolActivity(agent, sessionKey) {
		return ""
	}
	return toolActivitySummary(toolName, args)
}

// getShowToolActivity returns whether the session posts a one-line breadcrumb for
// each tool call, loading it from the session store on a cache miss. Default false.
func (al *AgentLoop) getShowToolActivity(agent *AgentInstance, sessionKey string) bool {
	key := activeModelCacheKey(agent.ID, sessionKey)

	al.showToolActivityMu.Lock()
	if v, ok := al.showToolActivityCache[key]; ok {
		al.showToolActivityMu.Unlock()
		return v
	}
	al.showToolActivityMu.Unlock()

	v := false
	if store, ok := agent.Sessions.(compactionStateStore); ok {
		if st, err := store.GetCompactionState(sessionKey); err == nil {
			v = st.ShowToolActivity
		}
	}

	al.showToolActivityMu.Lock()
	al.showToolActivityCache[key] = v
	al.showToolActivityMu.Unlock()
	return v
}

// setShowToolActivity sets the session's show-tool-activity flag, updating the
// cache and persisting it through CompactionState (best-effort).
func (al *AgentLoop) setShowToolActivity(agent *AgentInstance, sessionKey string, v bool) {
	key := activeModelCacheKey(agent.ID, sessionKey)
	al.showToolActivityMu.Lock()
	al.showToolActivityCache[key] = v
	al.showToolActivityMu.Unlock()

	if store, ok := agent.Sessions.(compactionStateStore); ok {
		st, err := store.GetCompactionState(sessionKey)
		if err != nil {
			logger.WarnCF("agent", "show tool activity: load compaction state failed",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
			return
		}
		st.ShowToolActivity = v
		if err := store.SetCompactionState(sessionKey, st); err != nil {
			logger.WarnCF("agent", "show tool activity: persist failed",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
			al.alertSessionStateNotPersisted(agent, "show tool activity", err)
		} else {
			al.storeFaultOver(agent, alertSessionStateNotStored)
		}
	}
}

// alertSessionStateNotPersisted raises the low alert for a per-session
// setting of agent's that stayed in memory only; what names the setting. A
// store that refuses one setting refuses the next: one alert, then one a day
// (alertStoreFault), until a setting is stored again.
func (al *AgentLoop) alertSessionStateNotPersisted(agent *AgentInstance, what string, err error) {
	al.alertStoreFault(agent, alerter.Alert{
		Title:       alertSessionStateNotStored,
		Description: what + ": the setting reverts on restart",
		Details:     err.Error(),
		EventID:     sessionStoreAlertID,
	})
}

// cogmemSessionStatus renders a short cognitive-memory summary for the session:
// active domain/memory counts, the pending-review count, and the last
// consolidation run. Returns "" when the agent is not cognitive. Opening the
// store runs the normal idempotent migrations, matching the cogmem_status tool.
func (al *AgentLoop) cogmemSessionStatus(ctx context.Context, agent *AgentInstance, sessionKey string) string {
	if agent == nil || agent.Config == nil || !agent.Config.CognitiveMemoryEnabled() {
		return ""
	}
	path := cogmemstore.DBPath(cogmemhost.Dir(agent.StateDir))
	if _, err := os.Stat(path); err != nil {
		return "No cognitive-memory database for this assistant yet."
	}
	s, err := cogmemstore.Open(path, cogmemhost.StoreOptions(agentModes(al.GetConfig(), path))...)
	if err != nil {
		return "Cognitive memory unavailable: " + err.Error()
	}
	defer utils.CloseQuietly(s)

	db := s.DB()
	var b strings.Builder

	active, err := s.ListDomains(ctx, db, cogmemstore.StatusActive)
	if err != nil {
		return "Cognitive memory unavailable: " + err.Error()
	}
	memCount := 0
	for _, d := range active {
		ms, listErr := s.ListMemories(ctx, db, d.ID, cogmemstore.StatusActive)
		if listErr != nil {
			return "Cognitive memory unavailable: " + listErr.Error()
		}
		memCount += len(ms)
	}
	fmt.Fprintf(&b, "Active domains: %d\n", len(active))
	fmt.Fprintf(&b, "Active memories: %d\n", memCount)

	run, ok, err := s.LastRun(ctx, db)
	if err != nil {
		return "Cognitive memory unavailable: " + err.Error()
	}
	if !ok {
		b.WriteString("Last consolidation: none yet")
	} else {
		when := run.StartedAt.Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "Last consolidation: %s — trigger=%s, status=%s, changes=%d",
			when, run.Trigger, run.Status, run.OpsApplied)
		if run.Error != "" {
			fmt.Fprintf(&b, "\nLast error: %s", run.Error)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
