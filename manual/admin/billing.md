# Billing codes

Every model call Agency makes is recorded with the agent it was for and a **billing code**, so you can see what each piece of work cost.

## How a code is chosen

- Each conversation has a billing code. It starts as `default`.
- An agent sets it with its `billing` tool, usually because you ask it to: "Bill this work to pentest-acme." The code applies from the agent's next model call and stays until it is changed, also after a restart.
- A code has 1 to 64 letters, digits, `-`, `_` or `.`. Case matters: `Acme` and `acme` are two codes.
- Work an agent starts takes the code in force when it starts: [sub-agents](../agents/subagents.md), [scheduled jobs](../agents/scheduling.md), agents it asks, [forum](../forums/index.md) participants, [Maestro](../maestro.md) tasks and [classifiers](../agents/classifiers.md). A sub-agent or Maestro task can be given its own code instead.
- Work started this way can change its own code with the `billing` tool. That never changes the code of the conversation that started it.
- A Maestro task's worker can change its own code only for that run: a retry, review or revision of the task uses the task's code again.
- Summarising a long conversation is billed to the conversation's code when it runs after a reply, and to the running work's code when that work waits on it.
- Memory consolidation is billed to the code `memory`. Scheduled jobs added with `agency cron add` use `default`.

Every agent has the `billing` tool. To take it away from one agent, add `billing` to its **Denied tools**; the agent can then no longer change its code, and its work stays billed to the codes it already has.

## The accounting log

The records are written to `<AGENCY_HOME>/logs/accounting.log`, one JSON object per line, with the [logs area's](./logs.md#file-permissions) file modes. Each line is one model call. A line for a sub-agent Bob that Alice started:

```json
{"time":"2026-11-23T09:14:02.123456Z","origin_agent":"alice","agent_id":"bob:1a2b3c4d","agent_name":"Bob","project":"pentest-acme","kind":"text","provider":"openai-chat","transport":"api","model":"gpt-4o","input_tokens":1200,"output_tokens":340,"cache_read_tokens":0,"cache_creation_tokens":"-","cost_usd":0.0064,"cost_source":"provided","tool_calls":2,"http_status":200,"duration_ms":4120,"success":true,"audio_seconds":"-","characters":"-","images":0}
```

The accounting log is not in the [backup](./backup.md), like the other logs.

### Who the call is for

| Field | What it holds |
|---|---|
| `origin_agent` | The agent whose work led to the call. Total by this field to see what each agent's work cost. |
| `agent_id` | The agent that made the call. |
| `agent_name` | That agent's name. |
| `project` | The billing code. |

`agent_id` has three forms:

| Form | Example | Meaning |
|---|---|---|
| An agent id | `bob` | The agent itself. |
| An agent id, `:` and eight characters | `bob:1a2b3c4d` | A temporary copy of that agent, such as a [sub-agent](../agents/subagents.md) or a [forum](../forums/index.md) participant. Each copy has its own eight characters. |
| `temp:` and eight characters | `temp:5b6c7d8e` | A temporary agent that is not a copy of one of yours, such as a forum participant given only a model. Its name is `temp`. |

`origin_agent` is the agent at the start of the chain of work:

| Work | `origin_agent` | `agent_id` |
|---|---|---|
| Alice answers a message | `alice` | `alice` |
| Alice starts a sub-agent Bob | `alice` | `bob:1a2b3c4d` |
| That sub-agent starts a sub-agent Carol | `alice` | `carol:9f8e7d6c` |
| Alice asks Bob | `alice` | `bob` |
| Bob takes part in a forum Alice launched | `alice` | `bob` |
| A [scheduled job](../agents/scheduling.md) Alice created runs as Bob | `alice` | `bob` |
| Bob's memory consolidation or [classifier](../agents/classifiers.md) | `bob` | `bob` |

When no agent started the work, `origin_agent` is the same as `agent_id`.

### What was called

| Field | What it holds |
|---|---|
| `time` | When the call started, in UTC. |
| `kind` | `text`, `image`, `speech` or `transcription`. |
| `provider` | The provider, such as `claude-cli`, `openai-chat` or `anthropic`. |
| `transport` | `cli` for a command-line model, `api` for a network call. |
| `model` | The model that answered, or the one asked for when the answer does not name it. |

### What it used and cost

| Field | What it holds |
|---|---|
| `input_tokens` | Input tokens, not counting those read from or written to the prompt cache. |
| `output_tokens` | Output tokens. |
| `cache_read_tokens` | Input tokens read from the prompt cache. |
| `cache_creation_tokens` | Input tokens written to the prompt cache. |
| `cost_usd` | The cost in US dollars. |
| `cost_source` | `provided` when the provider reported the cost, `estimated` when it came from your [pricing table](#pricing-table). |
| `tool_calls` | Tool calls the model made in this call. |
| `audio_seconds` | Seconds of audio billed. |
| `characters` | Characters of text sent to be spoken. |
| `images` | Images the call returned. |
| `notes` | Remarks on how the cost was worked out, such as the pricing row an estimate used. Left out when there are none. |

### How it went

| Field | What it holds |
|---|---|
| `http_status` | The HTTP status of the answer. |
| `duration_ms` | How long the call took, in milliseconds. |
| `success` | `true` or `false`. |
| `error` | The error. Left out when the call succeeded. |

A value that is not known is written as `-`, never as 0: a number the provider did not report, or the `http_status` of a command-line model. A 0 means the provider reported 0.

## Pricing table

When a provider does not report a call's cost, Agency can estimate it from a pricing table. Put it in `<AGENCY_HOME>/pricing.json`:

```json
{"prices": [
  {"model": "gpt-4o", "input": 2.5, "output": 10},
  {"model": "claude-*", "input": 3, "output": 15}
]}
```

Prices are US dollars per million tokens (`input`, `output`, `cache_read`, `cache_write`); speech, transcription and image models also take `characters` (per million), `audio_minute` and `image`. A model name ending in `*` matches every model starting with the text before it, and `*` alone matches any model. A misspelt field makes the file invalid.

The file is read when Agency starts and at every configuration reload. A file that cannot be read is logged as an error and the previous table stays in use.
