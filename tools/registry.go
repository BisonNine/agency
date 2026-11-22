package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/utils"
)

type ToolEntry struct {
	Tool Tool
	// IsCore is false for a tool hidden by progressive discovery: it is still
	// offered to the model on every call, but deferred (loaded only once a
	// search_tools or get_tool_details result references it), and it may be
	// called at any time.
	IsCore bool
	// SuiteExempt marks a tool that belongs to an all-or-nothing suite (cogmem,
	// maestro). Suites are gated as a unit by the per-agent suite flag at
	// registration time, so their tools must also bypass the execution-time
	// per-tool allowlist check (which has no entry for them).
	SuiteExempt bool
	// Group names the discovery set this tool belongs to (MCP server or fusion
	// service). RevealTogether, when set, makes get_tool_details on one of the
	// Group's hidden tools reference every hidden tool of the Group, so a small
	// cohesive set loads in one call. Both empty/false for ungrouped tools.
	Group          string
	RevealTogether bool
}

// ToolRegistry is one agent's tools: a thin wrapper over a toolkit catalogue
// that keeps the method set the host has always used and adds the host's own
// rules, the suite exemption and the per-call allow checker. The catalogue
// holds the tools; the wrapper records only which names are suite-exempt.
type ToolRegistry struct {
	cat *toolkit.Catalog

	// mu serialises the wrapper's writes and pairs a name's catalogue entry
	// with its suite exemption for a reader, so a call never sees one tool's
	// entry with another's exemption. Catalogue reads take no lock of their
	// own.
	mu     sync.RWMutex
	exempt map[string]bool // suite-exempt names, under mu
}

// toolkitLogger is the catalogue's logger: the host's SDK handler, with
// toolkit's lines logged under the "tool" component as they always were.
var toolkitLogger = logger.SDKAs("toolkit", "tool")

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		cat:    toolkit.New(toolkit.WithLogger(toolkitLogger), toolkit.WithTruncate(utils.Truncate)),
		exempt: make(map[string]bool),
	}
}

// put registers e under the write lock, recording whether its name is
// suite-exempt.
func (r *ToolRegistry) put(e toolkit.Entry, suiteExempt bool, what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.cat.Put(e); err != nil {
		logger.ErrorCF("tools", "Tool registration refused", map[string]any{"error": err.Error()})
		return
	}
	name := e.Tool.Name()
	if suiteExempt {
		r.exempt[name] = true
	} else {
		delete(r.exempt, name)
	}
	fields := map[string]any{"name": name}
	if e.Hidden {
		fields["group"] = e.Group
	}
	logger.DebugCF("tools", "Registered "+what, fields)
}

func (r *ToolRegistry) Register(tool Tool) {
	r.put(toolkit.Entry{Tool: tool}, false, "core tool")
}

// RegisterSuite registers a tool that belongs to an all-or-nothing suite. It is
// identical to Register but marks the name suite-exempt, so ExecuteWithContext
// skips the per-tool allowlist check (the suite flag is the allow decision).
func (r *ToolRegistry) RegisterSuite(tool Tool) {
	r.put(toolkit.Entry{Tool: tool}, true, "suite tool")
}

// RegisterSuiteHidden registers a suite tool that is ALSO hidden behind
// progressive discovery: suite-exempt from the per-tool allowlist (the suite flag
// is the allow decision) yet hidden like any discovery tool, so it is offered
// deferred until a discovery result references it. Used for the fusion
// and maestro suites when an agent has progressive discovery on.
func (r *ToolRegistry) RegisterSuiteHidden(tool Tool) {
	r.RegisterSuiteHiddenGroup(tool, "", false)
}

// RegisterSuiteHiddenGroup is RegisterSuiteHidden with a discovery group and
// reveal-together flag, so a suite's tools can be loaded as a set.
func (r *ToolRegistry) RegisterSuiteHiddenGroup(tool Tool, group string, revealTogether bool) {
	r.put(toolkit.Entry{Tool: tool, Hidden: true, Group: group, RevealTogether: revealTogether},
		true, "hidden suite tool")
}

// RegisterHidden saves a hidden tool: offered to the model deferred, loaded
// once a discovery result references it.
func (r *ToolRegistry) RegisterHidden(tool Tool) {
	r.RegisterHiddenGroup(tool, "", false)
}

// RegisterHiddenGroup is RegisterHidden with a discovery group and reveal-together
// flag, so an upstream server's (or service's) tools can be loaded as a set.
func (r *ToolRegistry) RegisterHiddenGroup(tool Tool, group string, revealTogether bool) {
	r.put(toolkit.Entry{Tool: tool, Hidden: true, Group: group, RevealTogether: revealTogether},
		false, "hidden tool")
}

// RemoveByPrefix removes every registered tool, visible or hidden, whose name
// starts with prefix, and returns how many were removed. The version moves
// when anything was removed so cached definitions and search indexes rebuild.
// Used to drop an upstream MCP server's tools ("mcp_<server>_") as a set before
// re-registering its current list, so a renamed or removed tool disappears.
func (r *ToolRegistry) RemoveByPrefix(prefix string) int {
	removed := r.ReplaceByPrefix(prefix, nil)
	if removed > 0 {
		logger.DebugCF("tools", "Removed tools by prefix", map[string]any{"prefix": prefix, "count": removed})
	}
	return removed
}

// ReplaceByPrefix swaps every registered tool whose name starts with prefix for
// entries, in one step: a concurrent reader (a turn listing its tools, a tool
// call) sees the old set or the new one, never neither. An entry whose name
// lacks the prefix is registered all the same; the caller keeps them in step.
// Returns how many tools were removed. The version moves when anything was
// removed or added.
func (r *ToolRegistry) ReplaceByPrefix(prefix string, entries []ToolEntry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	conv := make([]toolkit.Entry, 0, len(entries))
	for _, e := range entries {
		conv = append(conv, toolkit.Entry{
			Tool:           e.Tool,
			Hidden:         !e.IsCore,
			Group:          e.Group,
			RevealTogether: e.RevealTogether,
		})
	}
	_, removed := r.cat.ReplaceEntries(prefix, conv)
	for name := range r.exempt {
		if strings.HasPrefix(name, prefix) {
			delete(r.exempt, name)
		}
	}
	for _, e := range entries {
		if e.Tool == nil {
			continue
		}
		if e.SuiteExempt {
			r.exempt[e.Tool.Name()] = true
		} else {
			delete(r.exempt, e.Tool.Name())
		}
	}
	return removed
}

// Version changes whenever the registered tools may have changed. It is
// read from the catalogue's current generation, so it moves as soon as a
// write returns.
func (r *ToolRegistry) Version() uint64 {
	return r.cat.Version()
}

// HiddenToolSnapshot holds a consistent snapshot of hidden tools and the
// registry version at which it was taken. Used by BM25SearchTool cache.
type HiddenToolSnapshot struct {
	Docs    []HiddenToolDoc
	Version uint64
}

// HiddenToolDoc is a lightweight representation of a hidden tool for search indexing.
type HiddenToolDoc struct {
	Name        string
	Description string
}

// SnapshotHiddenTools returns all hidden tools and the registry version they
// were read at, both from one catalogue snapshot, so the two always agree.
func (r *ToolRegistry) SnapshotHiddenTools() HiddenToolSnapshot {
	snap := r.cat.Snapshot()
	hidden := snap.Hidden()
	docs := make([]HiddenToolDoc, 0, len(hidden))
	for _, info := range hidden {
		docs = append(docs, HiddenToolDoc{
			// The name ToProviderDefs offers the tool under, so a search hit
			// can be referenced and called as is.
			Name:        info.Published,
			Description: info.Tool.Description(),
		})
	}
	return HiddenToolSnapshot{Docs: docs, Version: snap.Version()}
}

// HiddenNamespaces returns the sorted, distinct leading namespaces of the
// currently hidden tools (e.g. "trello", "maestro", "browser") — the groups a
// model can discover via search_tools. Used to hint the available surface without
// listing every tool.
func (r *ToolRegistry) HiddenNamespaces() []string {
	set := make(map[string]struct{})
	for _, info := range r.cat.Hidden() {
		adv := advertisedName(info.Tool, info.Name)
		ns := adv
		if i := strings.IndexByte(adv, '_'); i > 0 {
			ns = adv[:i]
		}
		set[ns] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Get returns the tool registered under name (the internal registry key),
// hidden or not: progressive discovery only defers loading a tool's
// definition, it never makes a registered tool uncallable.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	info, ok := r.cat.Get(name)
	if !ok {
		return nil, false
	}
	return info.Tool, true
}

// advertisedName is a tool's bare ExternalName for MCP tools, else the
// internal registry name.
func advertisedName(t Tool, internal string) string {
	if en, ok := t.(ExternalNamer); ok {
		if ext := en.ExternalName(); ext != "" {
			return ext
		}
	}
	return internal
}

// toolDetails returns a tool's provider schema, the name it is offered under
// and the tool references a get_tool_details result carries for it. name may
// be the internal registry key or a bare ExternalName. A hidden tool is
// referenced, together with every other hidden tool of its group when the
// group is reveal-together (sorted after it); a core tool, already loaded,
// is not. ok is false when there is no such tool.
func (r *ToolRegistry) toolDetails(name string) (schema map[string]any, published string, refs []string, ok bool) {
	snap := r.cat.Snapshot()
	info, found := snap.Lookup(name)
	if !found {
		return nil, "", nil, false
	}
	published = info.Published
	if info.Hidden {
		refs = append(refs, published)
		if info.RevealTogether && info.Group != "" {
			var group []string
			for _, sib := range snap.Hidden() {
				if sib.Name != info.Name && sib.Group == info.Group {
					group = append(group, sib.Published)
				}
			}
			sort.Strings(group)
			refs = append(refs, group...)
		}
	}
	return ToolToSchema(info.Tool), published, refs, true
}

func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) *ToolResult {
	return r.ExecuteWithContext(ctx, name, args, "", "", nil)
}

// ExecuteUnattended runs a registered tool with no session behind it (a
// scheduled watch probe, a classifier run): it refuses a tool the registry
// does not hold (ErrToolNotRegistered) and a SessionScoped one
// (ErrToolSessionScoped), runs it through ExecuteWithContext with no channel,
// chat or async callback (so the context's allow checker applies), and turns
// an IsError result into an error carrying the tool's ForLLM text.
func (r *ToolRegistry) ExecuteUnattended(ctx context.Context, name string, args map[string]any) (string, error) {
	tool, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %q: %w", name, ErrToolNotRegistered)
	}
	if scoped, isScoped := tool.(SessionScoped); isScoped && scoped.IsSessionScoped() {
		return "", fmt.Errorf("tool %q: %w", name, ErrToolSessionScoped)
	}
	res := r.ExecuteWithContext(ctx, name, args, "", "", nil)
	if res.IsError {
		return "", fmt.Errorf("tool %q failed: %s", name, res.ForLLM)
	}
	return res.ForLLM, nil
}

// withCallAgent puts the agent the host's turn runs for (spawnllm's key) on
// ctx under toolkit's key, which the toolspec bridge reads.
func withCallAgent(ctx context.Context) context.Context {
	return toolkit.WithAgentID(ctx, providers.AgentIDFromContext(ctx))
}

// ExecuteWithContext executes a registered tool, hidden by discovery or not,
// with channel/chatID context and optional async callback.
// If the tool implements AsyncExecutor and a non-nil callback is provided,
// ExecuteAsync is called instead of Execute — the callback is a parameter,
// never stored as mutable state on the tool.
func (r *ToolRegistry) ExecuteWithContext(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID string,
	asyncCallback AsyncCallback,
) *ToolResult {
	// Resolve first so the model may call an MCP tool by its bare ExternalName
	// (the name it is advertised under) as well as the internal registry key.
	r.mu.RLock()
	info, found := r.cat.Lookup(name)
	suiteExempt := found && r.exempt[info.Name]
	r.mu.RUnlock()

	// Defense-in-depth: check tool allowlist from context before execution.
	// Suite tools (cogmem, maestro, fusion) are exempt from the ALLOW side — they
	// are gated as a unit by the per-agent suite flag at registration, not by the
	// per-tool allowlist — but the agent's deny list still applies to them. Gate
	// on the canonical (internal) name so MCP tools route to the mcp_tools
	// allow-list even when the caller used the bare ExternalName. A tool that is
	// not registered is left to the catalogue, which reports it not found.
	if checker := ToolAllowCheckerFromCtx(ctx); found && checker != nil {
		canonical := info.Name
		if checker.IsToolDenied(canonical) || (!suiteExempt && !checker.IsToolAllowed(canonical)) {
			// Tool arguments are intentionally NOT logged: they routinely
			// contain memory content, file contents, and other user data.
			logger.InfoCF("tool", "Tool execution started", map[string]any{"tool": name})
			logger.WarnCF("tool", "Tool execution denied by agent allowlist",
				map[string]any{"tool": canonical})
			return ErrorResult(NotEnabledMessage(name)).WithError(Refusal(fmt.Errorf("tool not permitted: %s", name)))
		}
	}

	// Who the call is for: tools read the chat via ToolChannel/ToolChatID and
	// bridged toolspec tools the agent id. Always set — tools validate what
	// they require.
	ctx = toolkit.WithChat(ctx, channel, chatID)
	ctx = withCallAgent(ctx)
	return r.cat.Execute(ctx, name, args, asyncCallback)
}

// GetDefinitions returns the schemas of the core tools (those not hidden by
// discovery), sorted by name.
func (r *ToolRegistry) GetDefinitions() []map[string]any {
	listed := r.cat.Listed()
	definitions := make([]map[string]any, 0, len(listed))
	for _, info := range listed {
		definitions = append(definitions, ToolToSchema(info.Tool))
	}
	return definitions
}

// ToProviderDefs converts tool definitions to provider-compatible format.
// This is the format expected by LLM provider APIs.
//
// Every registered tool is included, sorted by internal name, so the list is
// the same on every call while the registered set is unchanged (a stable
// prompt-cache prefix). A tool hidden by progressive discovery is marked
// Deferred: spawnllm loads it once a tool result references it
// (ToolResult.ToolReferences), natively where the provider can and otherwise
// by sending it as an ordinary tool from then on. search_tools is marked
// ToolSearch. With discovery off every tool is core and neither flag is set.
//
// MCP tools are advertised under their bare ExternalName ("<server>_<tool>") so a
// model sees a single naming convention across native, fusion, and upstream-MCP
// tools (matching what the MCP host publishes to CLI clients). Presenting the
// internal "mcp_" prefix here made it the lone prefixed outlier, so models routinely
// dropped it and the call was then rejected as unknown/not-permitted. The
// catalogue maps the bare name back for dispatch and gating.
func (r *ToolRegistry) ToProviderDefs() []providers.ToolDefinition {
	// One snapshot; schemas are built from it outside any lock, as a tool's
	// Description may read the registry itself (search_tools lists the hidden
	// namespaces).
	list := r.cat.Snapshot().List()
	definitions := make([]providers.ToolDefinition, 0, len(list))
	for _, info := range list {
		schema := ToolToSchema(info.Tool)

		// Safely extract nested values with type checks
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			continue
		}

		var desc string
		if v, ok := fn["description"].(string); ok {
			desc = v
		}
		var params map[string]any
		if v, ok := fn["parameters"].(map[string]any); ok {
			params = v
		}

		_, isSearch := info.Tool.(*SearchTool)
		definitions = append(definitions, providers.ToolDefinition{
			Type: "function",
			Function: providers.ToolFunctionDefinition{
				Name:        info.Published,
				Description: desc,
				Parameters:  params,
			},
			Deferred:   info.Hidden,
			ToolSearch: isSearch,
		})
	}
	return definitions
}

// List returns a list of all registered tool names.
func (r *ToolRegistry) List() []string {
	return r.cat.Names()
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	return len(r.cat.Names())
}

// GetSummaries returns human-readable summaries of the core tools (those not
// hidden by discovery). Returns a slice of "name - description" strings.
func (r *ToolRegistry) GetSummaries() []string {
	listed := r.cat.Listed()
	summaries := make([]string, 0, len(listed))
	for _, info := range listed {
		summaries = append(summaries, fmt.Sprintf("- `%s` - %s", info.Tool.Name(), info.Tool.Description()))
	}
	return summaries
}
