# Accounting identity: origin agent, agent id, agent name

Status: approved. Done: the SDK change (5.1, awaiting a tagged release) and
the config rules (5.4). To do: 5.2, 5.3, 5.5.

## 1. Goal

Every accounting record says whose spend it is and who did the work, in
fields a person can read and a script can group by without parsing.

Today a record has `agent` (the source agent's id for a clone) and
`agent_name` (a friendly name for a config agent, a log label such as
`alice (clone 7c6d5e4f)` for a clone). Nothing records which agent started the
work.

## 2. Agreed design

### 2.1 Fields

| Field | Holds | Examples |
|---|---|---|
| `origin_agent` | The agent at the top of the chain that caused the call. The working agent itself when nothing is above it. Never empty. | `alice` |
| `agent_id` | Who did the work. Replaces `agent`. | `alice`, `bob:1a2b3c4d`, `temp:1a2b3c4d` |
| `agent_name` | The friendly name of the agent that did the work. | `Alice`, `Bob`, `temp` |
| `project` | The billing code. Unchanged. | `garden` |

### 2.2 The `agent_id` forms

- 2.2a A configured agent: its id (`alice`).
- 2.2b A clone: `<source id>:<first 8 characters of the clone's UUID>`
  (`alice:7c6d5e4f`). The 8 characters are the ones ordinary logs already show.
- 2.2c A fresh temporary agent (no source): `temp:<first 8 characters>`.

### 2.3 Examples

| Case | `origin_agent` | `agent_id` | `agent_name` |
|---|---|---|---|
| Alice's own turn | `alice` | `alice` | Alice |
| Alice spawns a clone of itself | `alice` | `alice:7c6d5e4f` | Alice |
| Alice spawns a Bob worker | `alice` | `bob:1a2b3c4d` | Bob |
| That Bob worker spawns Carol | `alice` | `carol:9f8e7d6c` | Carol |
| Bob in Alice's forum, as himself | `alice` | `bob` | Bob |
| Model-only participant in Alice's forum | `alice` | `temp:5b6c7d8e` | temp |
| Scheduled job run as Alice | `alice` | `alice` | Alice |
| Job Alice scheduled to run as Bob | `alice` | `bob` | Bob |
| Forum launched by a tool client connected as Alice, Bob taking part | `alice` | `bob` | Bob |

### 2.4 Rules that follow

- 2.4a `origin_agent` is set once, where a chain starts, and never changes
  down the chain. The `billing` tool does not touch it.
- 2.4b `temp` becomes a reserved agent id: config refuses an agent called
  `temp`.
- 2.4c The longest agent id drops from 64 to 32 characters. With the suffix
  an `agent_id` is at most 41, inside the SDK's existing 64 limit.
- 2.4d No compatibility code and no migration: the limit and the reserved id
  are simply enforced.

## 3. User-visible changes (each to be confirmed before it is built)

- 3.1 Accounting log: `agent` renamed to `agent_id`; new `origin_agent`.
- 3.2 Accounting log: a clone's `agent_id` is `id:xxxxxxxx`, not the source id.
- 3.3 Accounting log: a clone's `agent_name` is the source's friendly name,
  not `alice (clone …)`.
- 3.4 Accounting log: a fresh temporary agent is `temp:xxxxxxxx` / `temp`.
- 3.5 Config: an agent id longer than 32 characters is refused.
- 3.6 Config: the agent id `temp` is refused.
- 3.7 Manual: `manual/admin/billing.md` gains a short table of the fields and
  the `:xxxxxxxx` suffix.

Not changed: ordinary logs keep the label `alice (clone 7c6d5e4f)`.

## 4. How the origin is carried (findings)

The billing code already travels down exactly the chain the origin must
follow, so the origin rides beside it. `agent/billing.go` is the one rule for
the code; work that runs outside the session that started it gets the code on
its context (`tools.WithBilling`, `tools/turnscope.go`).

Every place the code is handed on or stored needs the origin beside it:

| Path | Where the code is handed on today | Stored across a restart in |
|---|---|---|
| Sub-agent task | `tools/agents/subagent.go` `originOf` → `taskrun.go` `withTaskBilling` | task record (`taskstore.go`, `Billing`) |
| Task-model run | `tools/agents/taskmodels.go` | same task record |
| Ask (agent to agent) | `agent/ask.go:373`, message metadata (`bus.MetaBilling`) → `withInboundBilling` | the message |
| Forum participant | `agent/forum_host.go:181` | `forum.Origin` in `snapshot.json` |
| Scheduled job | `tools/schedule/listen.go:199`, `watch.go:144` | `cron.CronJob.Billing` |
| Classifier | `agent/classifier_host.go:239,324` | classifier definition |
| Maestro task | `tools/maestro/dispatcher.go:92` | Maestro's task (`Work.Billing`) |
| CLI-provider tool calls | `mcpserver/host.go:285` (`SetTurnScope`) | not stored |
| Replayed turn, `/` commands | `agent/loop_commands.go:125` | the session |

Where a record is produced, the identity comes from
`AgentInstance.Identity()` (`agent/instance.go:148`) plus `identityFor`
(`agent/billing.go:50`). Callers that build an identity directly, and so need
the origin too: `agent/vision_describe.go:122`, `agent/context_manager.go:253,302`
(summarising), `agent/loop_tools.go:231`, `agent/classifier_host.go:323`,
`agent/instance.go:799`, `agent/turn_agent.go:67`, `agent/task_models.go`.

## 5. Work

### 5.1 Agent SDK (`go.bison9.com/agent/accounting`), first

- 5.1a `Identity` gains `Origin`. `Normalize` sets an empty `Origin` to
  `Agent`, and applies the same length check to it.
- 5.1b `Record`: JSON key `agent` becomes `agent_id`; new `origin_agent`,
  written before `agent_id`.
- 5.1c The turn engine carries the origin per turn, as it does the project:
  `hook.Scope.Origin`, `loop.Spec.TurnOrigin` (`WithTurnOrigin`), asked once
  as a turn is set up and by the compaction run after a turn. A sub-agent's
  turn does not inherit it: the host names it through `TurnOrigin`.
- 5.1d README and tests updated. Tag a release; Agency moves to it.

### 5.2 Agency: identity

- 5.2a `AgentInstance.Identity()` returns the three forms of 2.2 and the
  friendly name of 2.1 (a clone takes its source's `DisplayName`).
- 5.2b `identityFor` sets `Origin` from the context, else the working
  agent's own configured id (a clone's source id).

### 5.3 Agency: carrying the origin

- 5.3a One context value beside the billing code (`tools/turnscope.go`),
  set where a chain starts and copied, never replaced, below it.
- 5.3b Each row of the table in 4 hands it on and, where it stores the
  billing code, stores the origin beside it.
- 5.3c A stored record without an origin (written before this change) falls
  back to the working agent. This is the same fallback as "nothing above
  it", not compatibility code.

### 5.4 Agency: config

- 5.4a `maxAgentIDLength` 64 → 32 (`config/agent_id.go`), including the
  repair and de-duplication helpers that cut to the limit.
- 5.4b `temp` refused by `ValidAgentID`'s callers, with a message naming the
  agent.
- 5.4c WebUI agent form: the same two limits where the id is typed.

- 5.4d The 32 limit and the reserved id apply to configured ids only (config
  load, `config.Store` update, the agent form, the setup wizard). The runtime
  rule (`ValidAgentID`, `NormalizeAgentID`) stays at 64, because a temporary
  agent's id is a 36-character UUID and passes through it.

### 5.5 Docs and tests

- 5.5a `manual/admin/billing.md`, `docs/architecture.md`. No changelog entry
  (see 6.4).
- 5.5b Tests: one per row of 2.3, asserting all three fields
  (`agent/identity_test.go` already checks agent, name and project per kind);
  origin survives a restart for a task, a forum and a scheduled job; config
  refuses a 33-character id and `temp`.
- 5.5c `make test`; `make check-webui` for the agent form.

## 6. Open questions

- 6.1 **Decided.** A scheduled job's origin is the agent at the top of the
  chain that created it (Alice schedules a job that runs as Bob: `alice`). The
  job stores it beside its billing code. A job added with `agency cron add`
  has nobody above it: the agent it runs as.
- 6.2 **Resolved: no Maestro change.** Every Maestro dispatch runs on the
  context of the `task_run` or `task_dispatch` call that requested it;
  Maestro never starts a run by itself (checked in v0.5.13). The origin
  reaches the dispatcher on that context.
- 6.3 **Decided.** Memory consolidation and classifiers have nobody above
  them: the origin is the agent they run for.
- 6.4 **Decided.** The application is released, but nothing from the
  refactor has shipped. Breaking changes made during the refactor are not
  noted as they are made; they are noted when the new version is released.
  So this work adds no changelog entry and no migration. For that release's
  notes: `agent` → `agent_id` and `origin_agent` in the accounting log, agent
  ids limited to 32 characters, `temp` reserved.
- 6.5 **Decided.** A forum is always launched by an agent (a tool client
  connects as one), and that agent is the origin agent of its participants'
  calls.
- 6.6 **Decided.** The SDK keeps one agent id per turn: a clone's turn runs
  in the SDK under its accounting id (`bob:1a2b3c4d`), so the SDK's own log
  lines and task view name it that way. Accepted; no second id in the SDK.
- 6.7 **Decided.** One vocabulary in the SDK, Maestro and Agency: origin
  agent, working agent, billing code. The SDK's "project" is renamed
  (`Identity.BillingCode`, `CheckBillingCode`, `TurnBillingCode`, record key
  `billing_code`), with no old names kept. Order: SDK release, Maestro
  release, then Agency.
