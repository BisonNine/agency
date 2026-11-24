# Tool Provider Layer — Developer Guide

This document explains how Agency's tool-provider layer works and gives a spec
for adding a new tool module (a package of related tools). Registering a module
once makes its tools available to **both** transports:

- **Internal tool calling** — the agent loop builds a per-agent tool registry and
  passes the tools to the LLM.
- **The MCP server** — `mcpserver` is handed the same per-agent registries,
  so every tool is also exposed over MCP (`/mcp`).

It also feeds the WebUI tool catalogue. You write a module once; all three get it.

---

## 1. Architecture

```
your package (tools/<ns>/)        ← implements toolspec.ToolProvider, BARE tool names
        │  RegisterTools(deps) []ToolDefinition
        ▼
toolspec  (transport-neutral, stdlib-only)
        │  ToolProvider / ToolDefinition / Parameter / ToolCall / Result / Deps
        ▼
tools.NamespacedProvider("<ns>", pkg.GlobalProvider)   ← applies the "<ns>_" prefix
        │  each definition becomes a toolkit.Tool through toolkit.FromSpec
        ▼
tools registry  (RegisterProvider / GetProviders)
        ├──► agent loop  → per-agent *tools.ToolRegistry  → LLM tool calling
        │                  (a wrapper over a toolkit.Catalog)
        ├──► mcpserver (WithAgentRegistries)               → MCP /mcp
        └──► web/backend/api/agents.go (Describe)          → WebUI catalogue
```

Key idea: a tool package is **transport-neutral and dependency-free** (it imports
only `toolspec`, which itself imports only the standard library). The package
returns tools with **bare names** (`"read_bytes"`). The *aggregator* mounts the package
under a **namespace** (`"file"`), and the published tool name is
`"<namespace>_<bare>"` (`"file_read_bytes"`). Namespacing lives in the aggregator, not
the package, so packages never collide and can be remounted under a new namespace
without code changes.

### The aggregator (where modules are wired in)

`internal/gateway/tool_providers.go` is the **one file** that imports every tool
package and registers it:

```go
func registerToolProviders() {
    tools.RegisterProvider(tools.NamespacedProvider("file", files.GlobalProvider))
    tools.RegisterProvider(tools.NamespacedProvider("web", toolsweb.GlobalProvider))
    tools.RegisterProvider(tools.NamespacedProvider("session", session.GlobalProvider))
    // … one line per module …
}
```

To add an internal module you (a) add its import and (b) add one
`RegisterProvider(NamespacedProvider("<ns>", pkg.GlobalProvider))` line here.

---

## 2. Core types (`go.bison9.com/agent/toolspec`)

The types live in the agent SDK's `toolspec` package. Abridged below: toolspec
also has `Group`/`RevealTogether` on `ToolDefinition` and `ConfigDir`/`DB` on
`Deps`.

```go
// A package implements this. deps is injected at build time.
type ToolProvider interface {
    RegisterTools(deps Deps) []ToolDefinition
}

// Everything needed to expose one tool. Name is BARE (no namespace).
type ToolDefinition struct {
    Name        string          // bare, e.g. "read"
    Description string
    Parameters  []Parameter     // declarative; the host derives JSON Schema
    RawSchema   map[string]any  // optional: a pre-built JSON Schema, used verbatim
    Hints       *ToolHints      // optional MCP annotations (read-only/destructive/…)
    Handler     ToolHandler

    SessionScoped bool          // needs ToolCall.Session populated
    Async         bool          // may use ToolCall.Notify / return Result.Async
    PrimaryOnly   bool          // vestigial: declared by toolspec, not enforced

    DefaultAllow *bool          // Allow(true) ⇒ in the "Default tools" set
    Category     string         // declared by toolspec, not read by Agency
    ConfigKey    string         // declared by toolspec, not read by Agency
}

type ToolHandler func(call *ToolCall) (*Result, error)

// Per-call bundle. Ctx lives on the call (net/http.Request precedent).
type ToolCall struct {
    Ctx     context.Context
    Args    map[string]any
    AgentID string
    Session string                 // resolved session key ("" if none)
    Channel string
    ChatID  string
    Notify  func(*Result)          // async late-delivery; nil ⇒ host has no async path
}

// Structured result. Hosts that only want text read ForLLM.
type Result struct {
    ForLLM  string   // required: model-facing content
    ForUser string   // optional: routed to the human by the host
    Silent  bool
    IsError bool
    Async   bool
    Media   []string // media-store refs
    Err     error    // internal detail; not the control-flow signal
}

// Dependency injection. Capabilities a host may not offer are nilable.
type Deps struct {
    Cfg       any    // *config.Config in Claw (type-assert)
    AgentID   string
    Workspace string
    Spawn     any    // global.Spawner; nil ⇒ host cannot spawn sub-agents
    Host      any    // host-specific strong deps; Claw passes tools.ToolDeps
}

// Optional metadata a host may use for gating/grouping.
type HostMeta interface {
    Namespace() string
    Description() string
    Available(cfg any) (ok bool, reason string)
}
```

`Parameter` is the declarative source of truth for one input
(`Name/Type/Required/Description/Enum/Default/Minimum/…`). `ParametersToSchema`
renders `[]Parameter` into a JSON Schema object; you rarely call it yourself —
the bridge does. Use `RawSchema` only when migrating a tool that already has a
hand-written schema.

### Default-allow

An agent has only the tools its own `tools` list names; there is no install-wide
per-tool switch. `DefaultAllow: toolspec.Allow(true)` only puts the tool in the
standard set the Agents page's **Default tools** button writes into a list
(`tools.DefaultEnabledToolNames`). Omit it for tools few agents need. `shell_exec`
is granted only by naming it: `"*"` and prefix entries never include it.
`shell_ctrl` is granted only with `shell_exec` (`config.ImpliedTools`), so the
shell provider's enumeration (`RegisterTools` with zero `Deps`) returns
`shell_exec` alone and its real build returns both.

### Sub-agents and the full toolset

**A sub-agent inherits the parent's full toolset.** There is no per-tool
sub-agent restriction: nothing is withheld from a worker because of what it is.

**A sub-agent runs on the agent's worker models.** `subagents.models` on the
agent, when set, is the chain a worker starts on and falls back along, and the
only models `agent_spawn`'s `model` may name (`AgentInstance.WorkerCandidates`,
resolved in `agent/instance.go`; the clone is created with
`agentreg.CloneModels`). When it is empty the worker uses the agent's own
`models`. Maestro dispatch goes through the same `RunSync` path.

`ToolDefinition.PrimaryOnly` still exists in `toolspec` and is vestigial —
**Agency does not read it**, so setting it has no effect. It once marked tools a
spawned worker could not have, which was effectively a depth limit of one, and
it was replaced by a real one: `agents.defaults.max_subagent_depth` (default 3,
`tools/agents/depth.go`). A primary turn is depth 0, the sub-agents it spawns run
at depth 1, and a spawn is refused once the spawning agent is already at the
bound. Sub-agents may therefore spawn further sub-agents and re-enter Maestro,
but a runaway chain cannot occur.

Two things that used to be handled by withholding tools are now handled where
they belong:

- **Recursion** is bounded by depth, as above, rather than by denying
  `agent_spawn`. Workers that legitimately need to fan out can.
- **Memory** is isolated by *scope*, not by tool availability. At spawn time the
  parent's cognitive memory is snapshotted onto the sub-agent's own private
  database, and the worker gets the memory **write** tools as well as the read
  ones — sub-agents specifically need them. The snapshot is a throwaway copy,
  deleted with the session when the worker finishes, so anything it writes stays
  on that copy and never reaches the parent's memory. See
  [Sub-agents](../manual/agents/subagents.md).

There is correspondingly no `tools.IsPrimaryOnly` helper: it was removed with
the mechanism.

---

## 3. Spec: adding a new tool module

### Step 1 — create the package

`tools/<ns>/` (short namespace, e.g. `weather`). Implement
`toolspec.ToolProvider` on an exported zero-value, and optionally `toolspec.HostMeta`:

```go
// tools/weather/global_provider.go
package weather

import (
    "fmt"

    "go.bison9.com/agent/toolspec"
)

// GlobalProvider is the exported entry point the aggregator mounts.
var GlobalProvider globalWeatherProvider

type globalWeatherProvider struct{}

// toolspec.HostMeta (optional but recommended): drives WebUI grouping + gating.
func (globalWeatherProvider) Namespace() string   { return "weather" }
func (globalWeatherProvider) Description() string  { return "Weather lookups" }
func (globalWeatherProvider) Available(cfg any) (bool, string) { return true, "" }

func (globalWeatherProvider) RegisterTools(deps toolspec.Deps) []toolspec.ToolDefinition {
    return []toolspec.ToolDefinition{
        {
            Name:         "forecast",                 // published as weather_forecast
            Description:  "Return the forecast for a city.",
            Category:     "weather",
            DefaultAllow: toolspec.Allow(true),         // omit/false ⇒ not in Default tools
            Parameters: []toolspec.Parameter{
                {Name: "city", Type: "string", Required: true, Description: "City name."},
                {Name: "days", Type: "integer", Description: "Days ahead (1-7).", Default: 1},
            },
            Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
                city, _ := call.Args["city"].(string)
                if city == "" {
                    return &toolspec.Result{IsError: true, ForLLM: "city is required"}, nil
                }
                text, err := fetchForecast(call.Ctx, city) // your logic
                if err != nil {
                    return nil, err                        // bridge marks IsError + ForLLM
                }
                return &toolspec.Result{ForLLM: text}, nil
            },
        },
    }
}
```

### Step 2 — register it in the aggregator

In `internal/gateway/tool_providers.go`:

```go
import "go.bison9.com/agency/tools/weather"

func registerToolProviders() {
    // … existing …
    tools.RegisterProvider(tools.NamespacedProvider("weather", weather.GlobalProvider))
}
```

That's it — `weather_forecast` is now available to agents (subject to the
allowlist), over MCP, and in the WebUI catalogue.

### Step 3 — add a test probe

`test.sh` / `tests/test_mcpserver.sh` exercise every MCP tool with the external
`probe` binary. Add your tool to Agency test config's exposed tool list and
add a probe case — success for hermetic tools, a graceful-error probe for
network/LLM tools.

---

## 4. Rules & contracts

- **Enumeration must be deps-free.** `Describe()` (WebUI/MCP cataloguing) calls
  `RegisterTools(toolspec.Deps{})` with a zero `Deps`. Return your full tool set
  and build handler **closures lazily** — never dereference `deps.Cfg`/`deps.Host`
  at the top of `RegisterTools` in a way that panics on nil. Construct real
  instances only when the dep is present (`c, _ := deps.Cfg.(*config.Config); if c != nil { … }`).
- **Bare names only.** Never prefix the namespace yourself; the aggregator does.
- **Return errors *and* set `Result`.** A returned `error` is for `errors.Is/As`;
  `Result.IsError`/`Result.ForLLM` are the model-facing outcome. The bridge folds
  a non-nil error into `IsError` + `ForLLM`, so returning `(nil, err)` is fine.
- **Recover strong host deps when needed.** Claw passes its rich
  `tools.ToolDeps` via `deps.Host`; recover it with
  `cd, _ := deps.Host.(tools.ToolDeps)` (gives `Workspace`, `AgentCfg`,
  `Dispatcher`, session closures, etc.). A pure module that needs none of that
  imports only `toolspec`.
- **Session-scoped tools** set `SessionScoped: true` and read `call.Session`.
- **Async tools** set `Async: true`, return `Result{Async: true}` immediately,
  and deliver the late result via `call.Notify(...)` — but check `call.Notify != nil`
  first and degrade gracefully when the host has no async path.
- **Schema:** prefer `Parameters`; use `RawSchema` only to carry an existing
  hand-written schema during migration.

### The per-agent registry

The tool core — `Tool` and its optional interfaces (`AsyncExecutor`,
`SessionScoped`, `ExternalNamer`; `DiscoveryGrouped`, which only discovery reads, is not re-exported), `ToolResult` and its
constructors, `Refusal`, the per-call context helpers (`WithToolContext`,
`ToolChannel`, `ToolChatID`, `WithSessionKey`, `ToolSessionKey`), the toolspec
bridge and the tool-call tally — is the SDK's `toolkit` package. `tools`
re-exports each under its historical name (`tools/toolkit.go`), so tool
packages import only `tools`.

`tools.ToolRegistry` is one agent's tools: a thin wrapper over a
`*toolkit.Catalog` that keeps the host's method set and adds its rules.

| Method | Catalogue call |
|---|---|
| `Register` | `Put(Entry{Tool})` |
| `RegisterEntries` | `Put(entry)` as given (a hidden one from `discovery.HideEntries`), with `Meta` marking a `SuiteExempt` one |
| `RegisterSuite` | the same `Put`, with `Meta` marking the tool suite-exempt |
| `ReplaceByPrefix` | `ReplaceEntries(prefix, …)` (entries as given, `SuiteExempt` in `Meta`), returning the removed count |
| `Get`, `List`, `Count`, `Version`, `Snapshot`, `Subscribe` | `Get(n).Tool`, `Names()`, `len(Names())`, `Version`, `Snapshot`, `Subscribe` |
| `Catalog` | the catalogue itself, which a turn's SDK agent runs (`Spec.Tools`) |
| `ExecuteUnattended` | `ExecuteUnattendedGated` with no chat and the same gate as `ExecuteWithContext` |
| `RegisterDiscoveryTools(ctx, …)` | `discovery.NewIndex(catalogue, LogQueries(true)).MetaTools()`, each tool `Put` suite-exempt unless denied |

Progressive discovery is the SDK's `discovery` package. The agent decides
which tools are discovery-eligible (fusion, maestro, upstream MCP);
`discovery.HideEntries` hides them, taking a tool's group from
`DiscoveryGrouped` when it has none, except those `always_shown_namespaces`
and the Maestro entry tool pin (`discovery.AlwaysShownFunc` asked with the
internal name, matched by `config.MatchVisibility`). Each registry has its
own `discovery.Index` behind `search_tools` and `get_tool_details`, so a
search answers from that agent's tools only, and the index rebuilds when the
catalogue changes. The tool list for a model call is the SDK's
`loop.ToolDefs(Snapshot(), revealed)`, the one the turn engine sends. Its
`Send` is every tool, hidden ones `Deferred`,
whatever the provider: the list is built before the fallback chain picks
one, and spawnllm leaves out what a provider cannot load natively.
Its `InContext`, for the tool-schema estimate, is the listed tools plus
those the request's history has revealed: the host keeps no revealed-tools
state, it builds a `discovery.Session` from the history's
`ToolReferences` for each estimate (`agent.revealedIn`).

`ExecuteWithContext` runs the tool with `ExecuteGated`: the gate applies the
agent's allow checker from the context to exactly the tool that will run
(`WithToolAllowChecker`: a denied tool is refused with `NotEnabledMessage`; a
suite tool skips the allow list but not the deny list). It also puts the chat (`toolkit.WithChat`) and the agent id of the turn
(`toolkit.WithAgentID`, which the toolspec bridge hands to handlers as
`call.AgentID`) on the context, and runs it through the catalogue.

A turn runs its calls on the catalogue through the SDK agent instead, with
the same effect: `Spec.ToolContext` puts the chat and agent id on the call
(`tools.CallContext`), then the allow checker and reply chat, and `Spec.Gate`
is `tools.AllowGate` for the name the model called (`agent/turn_agent.go`).

Changes are safe at any time, including mid-turn: a write is visible to the
next read as soon as it returns, the next tool list carries it, and a
call to a tool removed since the model's list returns an ordinary tool error.

---

## 5. Importing as an external module

`toolspec` is intentionally dependency-free
(standard library only), so the `ToolProvider`/`ToolDefinition` contract can be vendored or
imported by another codebase (e.g. Maestro/MCPFusion) without pulling in Claw.
A package written against `toolspec` alone — no `tools`, `config`, etc.
— is portable: any host that can adapt `toolspec.ToolProvider` into its own
registry can mount it. Claw's adapter is `tools.NamespacedProvider`; another host
would supply its own equivalent.

When a module needs Claw-specific dependencies, it reaches them through
`deps.Host.(tools.ToolDeps)` — which keeps the *portable* part (the tool
definitions and handlers' signatures) free of Claw imports while still letting
the handler bodies use Claw internals when running inside Claw.

### Spawning sub-agents from a module

A tool can launch sub-agent workers through the portable `global.Spawner` the
host injects into `Deps.Spawn`:

```go
sp, ok := deps.Spawn.(global.Spawner)
if ok && sp != nil {
    res, err := sp.Spawn(call.Ctx, global.SpawnRequest{
        Mode:    global.SpawnAndWait, // or SpawnCallback / SpawnDetached
        Task:    "summarize the attached log",
        Channel: call.Channel,
        ChatID:  call.ChatID,
        OnResult: call.Notify, // for SpawnCallback: re-inject the result on the channel
    })
    _ = res
    _ = err
}
```

The three modes are: `SpawnDetached` (fire-and-forget, result discarded),
`SpawnCallback` (fire-and-forget; the worker's result is delivered to
`OnResult` when it finishes — wire it to `call.Notify` to surface it on the
originating channel), and `SpawnAndWait` (run to completion and return the
worker's `Result` synchronously). `Spawner` lives in `global` (stdlib-only),
so an external module can depend on it without importing the rest of Claw; the
host supplies the concrete implementation.

---

## 6. Quick reference

| Thing | Location |
|---|---|
| Transport-neutral types | `go.bison9.com/agent/toolspec` (aliases in `global/tools.go`) |
| Tool core (`Tool`, results, context helpers, bridge) | `go.bison9.com/agent/toolkit` |
| Namespacing adapter | `tools/namespaced.go` (`NamespacedProvider`, over `toolkit.FromSpec`) |
| Provider list | `tools/providers.go` (`RegisterProvider` / `GetProviders`) |
| Per-agent registry | `tools/registry.go` (`ToolRegistry`, over a `toolkit.Catalog`) |
| Untrusted-content marking | `go.bison9.com/agent/toolkit/untrusted` |
| **Aggregator (wire new modules here)** | `internal/gateway/tool_providers.go` |
| Per-module entry points | `tools/<ns>/global_provider.go` (`var GlobalProvider`) |
| Sub-agent recursion bound | `agents.defaults.max_subagent_depth` (`tools/agents/depth.go`) |
| Host deps available to handlers | `tools` `ToolDeps` (via `deps.Host`) |
| MCP exposure | `mcpserver` (`WithAgentRegistries`) |
| WebUI catalogue | `web/backend/api/agents.go` (`GET /api/agents/tools`, `Describe`) |
| Test probes | `test.sh`, `tests/test_mcpserver.sh` |
