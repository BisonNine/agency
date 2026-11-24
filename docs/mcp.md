# MCP server: dual-endpoint authentication

Status: **implemented**. This document specifies how Agency's MCP server accepts
**standard bearer-token** auth alongside the existing in-call session-token
model, so that standard MCP clients and a comprehensive `probe`-based test suite
(mirroring Maestro's) can exercise every tool, while preserving Agency's
multi-assistant routing. `/mcp` is the bearer endpoint; `/internal` carries the
session-token parameter. See `mcpserver/{mcpserver,tools,bearer}.go`.

## Why two models

The current `session_token` parameter (required on every tool call) does **two**
jobs at once:

1. **Authentication** — proves the caller is permitted.
2. **Routing** — `Resolve(token) → {agentID, sessionKey, channel, chatID}` selects
   *which agent/session* the call belongs to (for `ForUser` delivery, cogmem
   scoping, the per-agent ACL, and cross-agent rejection).

That dual role is why a bearer token alone never fit: a bearer is conventionally
**one identity per connection**, whereas Agency needs **per-call routing** so a
single endpoint can serve many assistants/sessions. The token is not a generic
API key — it is a session handle that also authenticates.

The two consumer classes genuinely want different transports:

| Consumer | Needs | Fits |
|---|---|---|
| **Standard / external** — `probe`, generic MCP CLIs, test harness | one identity per connection, `Authorization: Bearer`, clean tool schemas | header (bearer) |
| **Internal multi-assistant** — Agency's CLI providers calling back | one configured endpoint, a different token per call | in-call parameter |

Rather than overload one knob, expose **two endpoints**, each with exactly one
auth model. A token resolves identically regardless of transport (the token store
is transport-agnostic), so **the same token works on both**.

## The two endpoints

### `/mcp` — standard (bearer) endpoint  ← the public, universal surface

- Auth: `Authorization: Bearer <token>` (HTTP header). Read per request via the
  mcp-go server's `HTTPContextFunc(ctx, *http.Request)` and placed in the request
  context; dispatch resolves it through the shared token store.
- **Clean tool schemas** — tools do **not** carry a `session_token` parameter
  here. Standard MCP clients and `probe` see normal tools; auth lives at the HTTP
  layer where standard tooling expects it. This is the key property that makes a
  comprehensive `probe` suite practical.
- A missing/invalid bearer is rejected at the HTTP layer with a proper **`401`**
  (not a tool-result error), matching standard MCP client expectations.
- `tools/list` lists only the tools the token's agent may call (its own tools,
  under its allowlists), each described as that agent's own tool, the same check
  a call goes through. On `/internal` the
  token is a parameter of each call, so `tools/list` there is the union of every
  agent's tools and the check happens at the call.

This becomes the default, documented endpoint — what external integrators point
at.

### `/internal` — session-token-parameter endpoint  ← Agency's multi-assistant routing

- Auth + routing: the existing required **`session_token`** parameter
  (`SST<64 hex>`) on every tool call, exactly as today.
- Used by Agency's own CLI providers (and any caller that must drive multiple
  sessions over one configured endpoint, distinguishing them per call).
- Behavior is unchanged from the current `/mcp`; it simply moves to `/internal`
  so the path name advertises its purpose.

> **Migration:** the current in-call-token behavior **moves from `/mcp` to
> `/internal`**. Any existing MCP client config that points at `/mcp` with the
> `session_token` parameter (notably Agency's per-agent CLI provider MCP config)
> must be repointed to `/internal`. `/mcp` is repurposed as the bearer endpoint.

## Token model (shared by both endpoints)

- One `SessionTokenStore`. `Resolve(token)` returns the `{agentID, sessionKey,
  archiveDir, channel, chatID}` record regardless of how the token arrived
  (parameter on `/internal`, or `Authorization: Bearer` on `/mcp`).
- The store lives for the whole process, not for one MCP server: the gateway
  creates it once and hands it to every server it builds (at start and on each
  config reload), so a reload does not invalidate the tokens already rendered
  into running turns and Maestro workers.
- Conversation and registered tokens are held by the agent SDK's
  `mcphost.Store` (issued in the `SST<64 hex>` format through its generator
  option); rotation per session, pinning, service tokens (kept hashed at rest)
  and the turn scope stay in `SessionTokenStore`.
- A bearer token **is** a session token, just transported in the header — not a
  new credential type. The same `SST<64 hex>` value is accepted both ways.
- Issuance: `Issue()` mints per-session tokens; `Register()` pre-mints a known
  token (used for tests); `RegisterService()` registers a long-lived per-agent
  **service token** for the `/mcp` bearer endpoint — the natural fit for "one
  identity per connection". It drives the agent's main session (one agent, one
  conversation, one memory — so the token carries the agent's privileges). See
  [service-tokens.md](service-tokens.md). For a `probe` run, register a known
  token and hand it to `probe` as the bearer.
- Cross-agent protection still applies: a token that resolves to one agent cannot
  act as another, on either endpoint.

## Implementation shape (high level — no code here)

- Two endpoints of the agent SDK's `mcphost` over the **same** agent
  registries, ACL policy, and token store (`mcpserver/mcpserver.go`,
  `mcpserver/host.go`):
  - `/internal`: an `Argument` endpoint; every published schema gains the
    required `session_token` argument, which is removed before the tool runs.
  - `/mcp`: a `Bearer` endpoint; the `Authorization: Bearer` token is checked on
    every request and a refused one is a 401 with realm `claw-mcp`.
- The host supplies who a token is (session and service tokens), each agent's
  view of its own registry (the endpoint's visibility filter; on `/mcp` the ACL
  too), the turn scope a call runs in, the ACL gate on the exact tool that runs,
  and what a call publishes on the bus. Both are served by the MCP host's own
  listener (`mcp_host.listen`, default `127.0.0.1:5911`), not the WebUI mux.
- Both are stateless HTTP: each request's credential routes consistently (and a
  single client could even drive multiple sessions via per-request bearers,
  though that is unusual).

## Testing (`probe`)

The bearer endpoint is what enables a Maestro-style comprehensive suite: point
`probe` at `/mcp`, authenticate with a registered bearer token, and exercise
every tool against clean schemas — no `session_token` threading. The existing
`tests/test_mcpserver.sh` (which uses the parameter model) continues to target
`/internal` unchanged, and a new bearer-based suite targets `/mcp`.

A parity test should assert the two endpoints expose the **same tool set** and
ACL behavior, differing only by the `session_token` parameter.

## Tool list refresh

The tools an external MCP server offers are listed when Agency connects to it,
registered onto every agent whose `mcp_tools` admits them, and published by the
host catalogue above. That list is refreshed, without restarting Agency, when:

- the server sends `notifications/tools/list_changed` (on a streamable HTTP
  connection under protocol 2026-07-28 Agency opens a `subscriptions/listen`
  stream for it; stdio and SSE connections carry it on their own stream);
- the liveness probe (`liveness_probe_seconds`, 60 by default, 0 disables)
  gets a `tools/list` answer that differs from the stored list;
- the server is reconnected — after a dropped connection, or by the
  **Reconnect** action on the WebUI's MCP servers page, which calls
  `POST /api/mcp/servers/{name}/reconnect` and forces the reconnect even
  through a post-failure cooldown.

The probe is the path to rely on. A server built on mcp-go (v1.1.0) cannot
push `tools/list_changed` to a client on the current protocol, so its tool
changes are seen only by the probe or a reconnect. Agency's own MCP host is
built on the same library and has the same limit towards *its* clients: a
client that listed Agency's tools at session start (Claude Code, for example)
does not learn of a catalogue change until it reconnects.

On a refresh the server's previous tools are removed from every agent and the
current list registered in their place, so a renamed or removed tool disappears
rather than lingering under its old name; the host catalogue is then brought in
step, and connected clients receive `tools/list_changed` for what was added or
deleted. A server that neither notifies nor is probed is refreshed only by a
reconnect.

## Security

- Both endpoints are bound to `127.0.0.1` by default (`mcp_host.listen`).
  The bearer header is no weaker than the in-call parameter over the same
  transport.
- Exposing either endpoint beyond localhost is a separate TLS decision, out of
  scope here.

## Why not "one endpoint, optional parameter"

Making `session_token` optional and accepting either the parameter or a bearer on
a single endpoint was considered and rejected:

- It marks a parameter **optional that is semantically required for routing** — a
  client (or LLM) reasonably omits it and then fails at runtime, instead of being
  structurally prevented.
- It cannot present **both** a clean (no-param) schema to standard clients and the
  optional param — so external clients still see the non-standard parameter,
  losing the main benefit.
- It mixes transport-layer and parameter-layer auth in one branchy code path:
  more failure modes, ambiguous errors ("missing token" — which kind?), and
  harder exhaustive testing.
- Documentation collapses to "pass A or B, but you must pass one" — ambiguous for
  both humans and LLMs.

Two endpoints keep each model crisp and independently testable.
