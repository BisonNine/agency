// Agency
// License: MIT

package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/mcphost"

	"go.bison9.com/agency/internal/tokenhash"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
	"go.bison9.com/agency/tools"
)

// sessionTokenPrefix is the magic literal at the start of every session token.
// Chosen to be distinct from the agent token prefix (AGT) and redactable by regex.
const sessionTokenPrefix = "SST"

// sessionRecord holds the mapping from a session token to its session. The
// chat a call's output goes to is not kept here: it is the chat of the turn
// running on the session (BeginTurn, ReplyChat).
type sessionRecord struct {
	agentID    string
	sessionKey string
	archiveDir string
	// homeID, when set, is the agent a late async result of this session is
	// delivered to instead of agentID: a temporary clone's source, resolved
	// when the token is issued, so it holds after the clone is deleted.
	homeID string
	// depth is the sub-agent depth of the session's current turn, recorded by
	// the agent loop when the turn starts (SetDepth). Tool calls presented with
	// this token run at it, so a CLI provider's spawns stay within
	// max_subagent_depth. Service tokens have no turn and stay at 0.
	depth int
	// askChain is the ask chain of the session's current turn
	// (SetTurnScope), applied to tool calls presented with this token like
	// depth. Service tokens have none.
	askChain []string
	// taskID is the task the session's current turn runs in (a sub-agent's
	// worker), recorded with the ask chain: a task a call with this token
	// starts is that task's own, cancelled with it.
	taskID string
	// billing is the inherited billing code of the session's current turn
	// (SetTurnScope), the turn's own value: a call with this token is billed
	// to it, and a billing tool call with it changes it for that turn. nil
	// when the turn bills to its session's code.
	billing *tools.InheritedBilling
	// service marks a per-agent service token (RegisterService,
	// SyncServiceTokens): it belongs to no turn, so a call with it has no
	// reply chat whatever turns its session runs.
	service bool
	// pinned marks a token that Issue() must never rotate away: registered test
	// tokens (Register) and long-lived per-agent service tokens (RegisterService).
	pinned bool
	// hashed marks a record keyed by tokenhash.Hash(token) rather than by the
	// token itself: service tokens loaded from disk, where only the hash is
	// kept. Resolve hashes the presented token to reach these, and never lets
	// the stored hash string itself authenticate.
	hashed bool
}

// SessionTokenStore maps SST<64hex> tokens to session records.
// Tokens are generated on first session use and rotated on clear.
//
// Session-scoped tools (get_session_messages, search_session_messages) require a
// session_token so the MCP server can inject the correct session key into the
// tool's execution context regardless of which HTTP request carries the call.
//
// Conversation and registered (test) tokens are held by the SDK's
// mcphost.Store, issued in the SST format (mcphost.WithGenerator), each grant's
// Value the sessionRecord. The rest is the host's: rotation per session key,
// pinning, the long-lived service tokens (kept hashed at rest), the turn scope
// a turn records on its token, and the running turns a call's chat comes from.
type SessionTokenStore struct {
	// sessions holds the conversation and registered tokens. Every change is
	// made under mu, so an index below and the store never disagree.
	sessions *mcphost.Store

	mu     sync.RWMutex
	bySess map[string]string // conversation sessionKey → token (rotation/revocation)
	// svc holds the long-lived service tokens, keyed by the token, or by its
	// hash when hashed. bySvc indexes them by agent id. They are tracked
	// separately from bySess because a service token resolves to the agent's
	// main session, which already has a conversation token: several tokens may
	// name one session, and rotating the conversation token must not disturb
	// the service token (or vice versa).
	svc   map[string]sessionRecord
	bySvc map[string]string // agentID → service token (its svc key)
	// home maps an agent to the agent its late results go to (a clone's
	// source); nil keeps every agent's own. Set once at startup.
	home func(agentID string) string
	// turns are the running turns per session key and the chat each replies
	// to (BeginTurn); nextTurn numbers them.
	turns    map[string]map[uint64]turnDest
	nextTurn uint64
}

// SetHomeResolver installs the lookup from an agent to the agent its late
// async results are delivered to (a temporary clone's source). Tokens issued
// afterwards record it.
func (s *SessionTokenStore) SetHomeResolver(fn func(agentID string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.home = fn
}

// NewSessionTokenStore returns an empty store. The gateway creates one for the
// life of the process and hands it to every MCP server it builds (see
// WithSessionTokenStore), so a config reload does not invalidate the tokens
// already rendered into running prompts.
func NewSessionTokenStore() *SessionTokenStore { return newSessionTokenStore() }

func newSessionTokenStore() *SessionTokenStore {
	return &SessionTokenStore{
		sessions: mcphost.NewStore(mcphost.WithGenerator(generateSessionToken)),
		bySess:   make(map[string]string),
		svc:      make(map[string]sessionRecord),
		bySvc:    make(map[string]string),
	}
}

// grantFor is the mcphost grant of a conversation or registered token.
func grantFor(rec sessionRecord) mcphost.Grant {
	return mcphost.Grant{Scope: hook.Scope{AgentID: rec.agentID, Session: rec.sessionKey}, Value: rec}
}

// sessionRecordOf returns the record a token of the sessions store stands
// for.
func (s *SessionTokenStore) sessionRecordOf(token string) (sessionRecord, bool) {
	g, _, ok := s.sessions.Resolve(token)
	if !ok {
		return sessionRecord{}, false
	}
	rec, ok := g.Value.(sessionRecord)
	return rec, ok
}

// Issue generates a new token for the given session and stores the mapping.
// If a token already exists for this sessionKey, it is revoked first.
// Returns the new SST<64hex> token.
func (s *SessionTokenStore) Issue(agentID, sessionKey, archiveDir string) string {
	// Resolve the home agent outside the store's lock: the resolver reads the
	// agent registry.
	s.mu.RLock()
	home := s.home
	s.mu.RUnlock()
	homeID := ""
	if home != nil {
		if h := home(agentID); h != "" && h != agentID {
			homeID = h
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec := sessionRecord{agentID: agentID, sessionKey: sessionKey, archiveDir: archiveDir, homeID: homeID}

	// If a pinned token is already registered for this session key, preserve it —
	// pinned tokens (test + service tokens) must not be rotated by normal session
	// activity. Return the existing token so the caller (getContextManager) can
	// store it in the system prompt if needed.
	old, hadOld := s.bySess[sessionKey]
	if hadOld {
		prior, _ := s.sessionRecordOf(old)
		if prior.pinned {
			return old
		}
		// Keep the running turn's scope across rotation.
		rec.depth = prior.depth
		rec.askChain = prior.askChain
		rec.taskID = prior.taskID
	}
	tok, err := s.sessions.Issue(grantFor(rec))
	if err != nil {
		// crypto/rand failure is catastrophic; return empty so callers can fail
		// closed rather than binding to a predictable value.
		return ""
	}
	if hadOld {
		s.sessions.Revoke(old)
	}
	s.bySess[sessionKey] = tok
	logger.InfoCF("mcpserver", "session token issued",
		map[string]any{"agent": agentID, "session": sessionKey})
	return tok
}

// Register stores a pre-specified token → session mapping. Unlike Issue,
// it does not generate a new token — the caller supplies the exact token
// string. If a token already exists for this sessionKey, it is revoked first.
// Intended for test setups where a known token must be registered before
// any LLM session has started.
func (s *SessionTokenStore) Register(token, agentID, sessionKey, archiveDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Revoke any existing token for this session key, and the token itself
	// if it stands for another session: the token now names this one.
	if old, ok := s.bySess[sessionKey]; ok {
		s.sessions.Revoke(old)
	}
	s.sessions.Revoke(token)

	rec := sessionRecord{agentID: agentID, sessionKey: sessionKey, archiveDir: archiveDir, pinned: true}
	if err := s.sessions.Register(token, grantFor(rec)); err != nil {
		// Only an empty token is refused here: the store held neither.
		logger.WarnCF("mcpserver", "session token not registered",
			map[string]any{"agent": agentID, "session": sessionKey, "error": err.Error()})
		delete(s.bySess, sessionKey)
		return
	}
	s.bySess[sessionKey] = token
}

// RegisterService registers a long-lived per-agent service token bound to the
// agent's main session (agent:<id>:main): an agent has one conversation,
// whatever is driving it. Like Register it is pinned (never rotated by Issue)
// and it is indexed apart from the conversation token, so neither disturbs the
// other. The caller supplies the exact token (minted/persisted by the
// `claw token` CLI).
func (s *SessionTokenStore) RegisterService(token, agentID, archiveDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionKey := routing.BuildAgentMainSessionKey(agentID)
	// Replace this agent's previous service token, and only that: the session's
	// conversation token (bySess) is a separate credential and stays put, so
	// registering a service token can never revoke the agent's own token.
	if old, ok := s.bySvc[agentID]; ok {
		delete(s.svc, old)
	}
	s.svc[token] = sessionRecord{
		agentID:    agentID,
		sessionKey: sessionKey,
		archiveDir: archiveDir,
		pinned:     true,
		service:    true,
	}
	s.bySvc[agentID] = token
	logger.InfoCF("mcpserver", "service token registered",
		map[string]any{"agent": agentID, "session": sessionKey})
}

// SyncServiceTokens reconciles the live store to the given agentID→token set:
// agents present have their service token (re)registered; service tokens for
// agents no longer present are revoked. archiveDirFor maps an agentID to its
// archive dir; agents it returns "" for (unknown) are skipped. This is what lets
// `claw token` changes take effect without a restart. The values are the
// stored form from servicetoken.Load — hashes — so the record is keyed by the
// hash and Resolve hashes the presented token to find it; a plaintext value
// (tests) is keyed as-is.
func (s *SessionTokenStore) SyncServiceTokens(tokens map[string]string, archiveDirFor func(agentID string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop every existing service token. They are indexed by agent, so this never
	// touches a conversation token, though both name the agent's main session.
	for agentID, tok := range s.bySvc {
		delete(s.svc, tok)
		delete(s.bySvc, agentID)
	}
	// Register the current set.
	for agentID, tok := range tokens {
		archiveDir := archiveDirFor(agentID)
		if archiveDir == "" {
			continue
		}
		s.svc[tok] = sessionRecord{
			agentID:    agentID,
			sessionKey: routing.BuildAgentMainSessionKey(agentID),
			archiveDir: archiveDir,
			pinned:     true,
			service:    true,
			hashed:     tokenhash.IsHashed(tok),
		}
		s.bySvc[agentID] = tok
	}
}

// turnDest is the chat a running turn's final reply goes to.
type turnDest struct {
	channel, chatID string
}

// BeginTurn records a turn starting on sessionKey whose final reply goes to
// chatID on channel, until the returned end is called (once; later calls do
// nothing). A conversation-token call while exactly one turn of its session
// runs has that turn's reply chat (ReplyChat), where all of the call's
// output for "the current chat" goes; turns are kept per turn, not per
// session, so concurrent turns (an ask beside a chat turn) never take each
// other's chat.
func (s *SessionTokenStore) BeginTurn(sessionKey, channel, chatID string) (end func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turns == nil {
		s.turns = make(map[string]map[uint64]turnDest)
	}
	s.nextTurn++
	id := s.nextTurn
	if s.turns[sessionKey] == nil {
		s.turns[sessionKey] = make(map[uint64]turnDest)
	}
	s.turns[sessionKey][id] = turnDest{channel: channel, chatID: chatID}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.turns[sessionKey], id)
			if len(s.turns[sessionKey]) == 0 {
				delete(s.turns, sessionKey)
			}
		})
	}
}

// ReplyChat returns the reply chat of a call made with rec: the running
// turn of its session when there is exactly one, Ambiguous when there are
// more, and false for a service token or a session with no turn running.
func (s *SessionTokenStore) ReplyChat(rec sessionRecord) (tools.ReplyChat, bool) {
	if rec.service {
		return tools.ReplyChat{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	turns := s.turns[rec.sessionKey]
	switch len(turns) {
	case 0:
		return tools.ReplyChat{}, false
	case 1:
		for _, d := range turns {
			return tools.ReplyChat{Channel: d.channel, ChatID: d.chatID}, true
		}
	}
	return tools.ReplyChat{Ambiguous: true}, true
}

// SetDepth records the sub-agent depth of the turn now running on sessionKey
// on its conversation token, so MCP tool calls made with that token (a CLI
// provider's) run at the turn's depth. Called by the agent loop at the start of
// every turn, so a later turn at a lower depth lowers it again. Service tokens
// are indexed apart and never carry a depth. No-op if the session has no
// token.
func (s *SessionTokenStore) SetDepth(sessionKey string, depth int) {
	s.updateRecord(sessionKey, func(rec *sessionRecord) { rec.depth = depth })
}

// SetTurnScope records the ask chain of the turn now running on sessionKey,
// the task it runs in and its inherited billing code on its conversation
// token, so MCP tool calls made with it (a CLI provider's) carry them as
// in-process tool calls do: an agent waiting in the exchange cannot be
// asked, a task the call starts is cancelled with the task it was started
// in, and the call is billed to the turn's code (nil: the session's).
// Called by the agent loop at the start of every turn. No-op if the session
// has no token.
func (s *SessionTokenStore) SetTurnScope(sessionKey string, askChain []string, taskID string, billing *tools.InheritedBilling) {
	chain := slices.Clone(askChain)
	s.updateRecord(sessionKey, func(rec *sessionRecord) { rec.askChain, rec.taskID, rec.billing = chain, taskID, billing })
}

// updateRecord applies update to the conversation token record of
// sessionKey under the store lock. No-op when the session has no token.
func (s *SessionTokenStore) updateRecord(sessionKey string, update func(*sessionRecord)) {
	if sessionKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.bySess[sessionKey]
	if !ok {
		return
	}
	g, _, ok := s.sessions.Resolve(tok)
	if !ok {
		return
	}
	rec, ok := g.Value.(sessionRecord)
	if !ok {
		return
	}
	update(&rec)
	s.sessions.Rescope(tok, g.Scope, rec)
}

// Resolve looks up a presented token. Returns the record and true if found.
// A record stored under the token's hash is reached by hashing the presented
// value; presenting the hash string itself matches nothing.
func (s *SessionTokenStore) Resolve(token string) (sessionRecord, bool) {
	if rec, ok := s.sessionRecordOf(token); ok {
		return rec, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec, ok := s.svc[token]; ok && !rec.hashed {
		return rec, true
	}
	if rec, ok := s.svc[tokenhash.Hash(token)]; ok && rec.hashed {
		return rec, true
	}
	return sessionRecord{}, false
}

// Revoke removes the token for a given session key (called on clear/eviction).
func (s *SessionTokenStore) Revoke(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.bySess[sessionKey]; ok {
		s.sessions.Revoke(tok)
		delete(s.bySess, sessionKey)
	}
}

// RevokeAgent removes all tokens for a given agent (called on agent removal).
func (s *SessionTokenStore) RevokeAgent(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sessionKeys []string
	s.sessions.RevokeFunc(func(g mcphost.Grant) bool {
		if g.Scope.AgentID != agentID {
			return false
		}
		sessionKeys = append(sessionKeys, g.Scope.Session)
		return true
	})
	for _, key := range sessionKeys {
		delete(s.bySess, key)
	}
	for tok, rec := range s.svc {
		if rec.agentID == agentID {
			delete(s.bySess, rec.sessionKey)
			delete(s.svc, tok)
		}
	}
	delete(s.bySvc, agentID)
}

// generateSessionToken returns "SST" + 64 lowercase hex characters (32 random bytes).
func generateSessionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sessiontoken: crypto/rand read: %w", err)
	}
	return sessionTokenPrefix + hex.EncodeToString(raw), nil
}
