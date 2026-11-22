package tools

import (
	"context"
	"fmt"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/toolkit"

	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/utils"
)

// ToolEntry is one tool to register: its catalogue entry and whether it is
// suite-exempt. A tool hidden by progressive discovery has an entry from
// discovery.HideEntries (Hidden, with its Group and RevealTogether): it is
// still offered to the model on every call, but deferred (loaded only once a
// search_tools or get_tool_details result references it), and it may be called
// at any time. The entry's Meta is the registry's own and is replaced.
type ToolEntry struct {
	toolkit.Entry
	// SuiteExempt marks a tool that belongs to an all-or-nothing suite (cogmem,
	// maestro). Suites are gated as a unit by the per-agent suite flag at
	// registration time, so their tools must also bypass the execution-time
	// per-tool allowlist check (which has no entry for them).
	SuiteExempt bool
}

// ToolRegistry is one agent's tools: a thin wrapper over a toolkit catalogue
// that keeps the method set the host has always used and adds the host's own
// rules, the suite exemption and the per-call allow checker. Whether a tool is
// suite-exempt travels with its catalogue entry (Entry.Meta), so a call is
// checked against exactly the tool that runs.
type ToolRegistry struct {
	cat *toolkit.Catalog
}

// suiteExempt is the Meta of a tool registered as part of an all-or-nothing
// suite; any other tool has none.
type suiteExempt struct{}

// entryMeta is the Meta for a tool that is, or is not, suite-exempt.
func entryMeta(exempt bool) any {
	if exempt {
		return suiteExempt{}
	}
	return nil
}

// isSuiteExempt reports whether info is a suite-exempt tool.
func isSuiteExempt(info toolkit.Info) bool {
	_, ok := info.Meta.(suiteExempt)
	return ok
}

// toolkitLogger is the catalogue's logger: the host's SDK handler, with
// toolkit's lines logged under the "tool" component as they always were.
var toolkitLogger = logger.SDKAs("toolkit", "tool")

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		cat: toolkit.New(toolkit.WithLogger(toolkitLogger), toolkit.WithTruncate(utils.Truncate)),
	}
}

// put registers e, its Meta recording whether it is suite-exempt.
func (r *ToolRegistry) put(e toolkit.Entry, exempt bool, what string) {
	e.Meta = entryMeta(exempt)
	if err := r.cat.Put(e); err != nil {
		logger.ErrorCF("tools", "Tool registration refused", map[string]any{"error": err.Error()})
		return
	}
	fields := map[string]any{"name": e.Tool.Name()}
	if e.Hidden {
		fields["group"] = e.Group
	}
	logger.DebugCF("tools", "Registered "+what, fields)
}

func (r *ToolRegistry) Register(tool Tool) {
	r.put(toolkit.Entry{Tool: tool}, false, "core tool")
}

// RegisterSuite registers a tool that belongs to an all-or-nothing suite. It is
// identical to Register but marks the entry suite-exempt, so ExecuteWithContext
// skips the per-tool allowlist check (the suite flag is the allow decision).
func (r *ToolRegistry) RegisterSuite(tool Tool) {
	r.put(toolkit.Entry{Tool: tool}, true, "suite tool")
}

// RegisterEntries registers each entry as it is: a tool hidden by discovery
// (an entry from discovery.HideEntries) is offered deferred until a discovery
// result references it; a suite-exempt one skips the per-tool allowlist (its
// suite flag is the allow decision). The fusion and maestro suites register
// this way when progressive discovery is on.
func (r *ToolRegistry) RegisterEntries(entries ...ToolEntry) {
	for _, e := range entries {
		what := "core tool"
		switch {
		case e.Hidden && e.SuiteExempt:
			what = "hidden suite tool"
		case e.Hidden:
			what = "hidden tool"
		case e.SuiteExempt:
			what = "suite tool"
		}
		r.put(e.Entry, e.SuiteExempt, what)
	}
}

// RegisterDiscoveryTools registers progressive discovery's meta-tools,
// search_tools and get_tool_details (discovery.MetaTools), answering from
// this registry's hidden tools with at most maxResults hits per search. They
// are suite-exempt (gated by the discovery switch, not the per-tool
// allowlist); one that skip reports true for (the agent's deny list) is left
// out.
func (r *ToolRegistry) RegisterDiscoveryTools(ctx context.Context, maxResults int, skip func(name string) bool) {
	ix := discovery.NewIndex(r.cat, discovery.MaxResults(maxResults), discovery.WithLogger(logger.SDK()), discovery.LogQueries(true))
	entries, err := ix.MetaTools().Tools(ctx)
	if err != nil {
		logger.ErrorCF("tools", "Tool registration refused", map[string]any{"error": err.Error()})
		return
	}
	for _, e := range entries {
		if skip != nil && skip(e.Tool.Name()) {
			continue
		}
		r.put(e, true, "suite tool")
	}
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
	conv := make([]toolkit.Entry, 0, len(entries))
	for _, e := range entries {
		ce := e.Entry
		ce.Meta = entryMeta(e.SuiteExempt)
		conv = append(conv, ce)
	}
	_, removed := r.cat.ReplaceEntries(prefix, conv)
	return removed
}

// Version changes whenever the registered tools may have changed. It is
// read from the catalogue's current generation, so it moves as soon as a
// write returns.
func (r *ToolRegistry) Version() uint64 {
	return r.cat.Version()
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

func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) *ToolResult {
	return r.ExecuteWithContext(ctx, name, args, "", "", nil)
}

// ExecuteUnattended runs a registered tool with no session behind it (a
// scheduled watch probe, a classifier run): it refuses a tool the registry
// does not hold (ErrToolNotRegistered) and a SessionScoped one
// (ErrToolSessionScoped), applies the context's allow checker as
// ExecuteWithContext does, passes no channel, chat or async callback, and
// turns an IsError result into an error carrying the tool's ForLLM text.
func (r *ToolRegistry) ExecuteUnattended(ctx context.Context, name string, args map[string]any) (string, error) {
	ctx = withCallAgent(toolkit.WithChat(ctx, "", ""))
	return r.cat.ExecuteUnattendedGated(ctx, name, args, allowGate(name))
}

// withCallAgent puts the agent the host's turn runs for (spawnllm's key) on
// ctx under toolkit's key, which the toolspec bridge reads.
func withCallAgent(ctx context.Context) context.Context {
	return toolkit.WithAgentID(ctx, providers.AgentIDFromContext(ctx))
}

// allowGate is the host's per-call grant check, run by the catalogue on the
// exact tool a call resolved to (name is the name the caller used). It is
// defense in depth over registration: the agent's allow checker on the
// context refuses a tool it denies, and one it does not allow unless the
// tool is suite-exempt (cogmem, maestro, fusion are gated as a unit by the
// per-agent suite flag at registration). The check uses the internal name,
// so MCP tools route to the mcp_tools allow-list even when called by their
// bare ExternalName. With no checker on the context every tool may run.
func allowGate(name string) toolkit.Gate {
	return func(ctx context.Context, info toolkit.Info) *toolkit.Result {
		checker := ToolAllowCheckerFromCtx(ctx)
		if checker == nil {
			return nil
		}
		if checker.IsToolDenied(info.Name) || (!isSuiteExempt(info) && !checker.IsToolAllowed(info.Name)) {
			logger.WarnCF("tool", "Tool execution denied by agent allowlist",
				map[string]any{"tool": info.Name})
			return ErrorResult(NotEnabledMessage(name)).WithError(Refusal(fmt.Errorf("tool not permitted: %s", name)))
		}
		return nil
	}
}

// ExecuteWithContext executes a registered tool, hidden by discovery or not,
// with channel/chatID context and optional async callback. The model may name
// an MCP tool by its bare ExternalName as well as its internal name. If the
// tool implements AsyncExecutor and a non-nil callback is provided,
// ExecuteAsync is called instead of Execute — the callback is a parameter,
// never stored as mutable state on the tool. Tool arguments are never logged.
func (r *ToolRegistry) ExecuteWithContext(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID string,
	asyncCallback AsyncCallback,
) *ToolResult {
	// Who the call is for: tools read the chat via ToolChannel/ToolChatID and
	// bridged toolspec tools the agent id. Always set — tools validate what
	// they require.
	ctx = withCallAgent(toolkit.WithChat(ctx, channel, chatID))
	return r.cat.ExecuteGated(ctx, name, args, asyncCallback, allowGate(name))
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

// ToolDefs is the tool list for one model call (discovery.Defs in the
// provider type).
type ToolDefs struct {
	// Send is the list given to the provider.
	Send []providers.ToolDefinition
	// InContext is what reaches the model's context, and so what the tool
	// schemas cost: the tools that are not deferred, then the deferred ones
	// revealed has seen, in order of first reveal.
	InContext []providers.ToolDefinition
}

// ToolDefs returns the tool list for one model call, from one snapshot of the
// registry (discovery.ToolDefs). revealed is what the conversation's tool
// results have referenced (nil for nothing); it only shapes InContext.
//
// Send is every registered tool, sorted by internal name, so it is the same
// on every call while the registered set is unchanged (a stable prompt-cache
// prefix). A tool hidden by progressive discovery is marked Deferred, as for
// a provider that loads deferred tools natively: the list is built before the
// fallback chain picks the provider, and spawnllm loads a deferred tool once a
// tool result references it (ToolResult.ToolReferences), natively where the
// provider can and otherwise by sending it as an ordinary tool from then on.
// search_tools is marked ToolSearch and its description names the hidden
// namespaces. With discovery off nothing is hidden and only ToolSearch can
// be set.
//
// MCP tools are advertised under their bare ExternalName ("<server>_<tool>") so a
// model sees a single naming convention across native, fusion, and upstream-MCP
// tools (matching what the MCP host publishes to CLI clients). Presenting the
// internal "mcp_" prefix here made it the lone prefixed outlier, so models routinely
// dropped it and the call was then rejected as unknown/not-permitted. The
// catalogue maps the bare name back for dispatch and gating.
func (r *ToolRegistry) ToolDefs(revealed *discovery.Session) ToolDefs {
	defs := discovery.ToolDefs(r.cat.Snapshot(), revealed, true)
	return ToolDefs{Send: providerDefs(defs.Send), InContext: providerDefs(defs.InContext)}
}

// providerDefs copies discovery's definitions into the provider type.
func providerDefs(in []discovery.Def) []providers.ToolDefinition {
	out := make([]providers.ToolDefinition, 0, len(in))
	for _, d := range in {
		out = append(out, providers.ToolDefinition{
			Type: "function",
			Function: providers.ToolFunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
			Deferred:   d.Deferred,
			ToolSearch: d.ToolSearch,
		})
	}
	return out
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
