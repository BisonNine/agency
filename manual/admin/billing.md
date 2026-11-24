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

The records are written to `<AGENCY_HOME>/logs/accounting.log`, one JSON object per line, with the [logs area's](./logs.md#file-permissions) file modes. A line, shortened:

```json
{"time":"2026-11-23T09:14:02.123456Z","agent":"alice","agent_name":"Alice","project":"pentest-acme","kind":"text","model":"gpt-4o","input_tokens":1200,"output_tokens":340,"cost_usd":0.0064,"cost_source":"provided","success":true}
```

`project` is the billing code. `cost_source` says whether the cost came from the provider (`provided`) or from your pricing table (`estimated`); `-` means it is not known. A number the provider did not report is written as `-`, never as 0.

The accounting log is not in the [backup](./backup.md), like the other logs.

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
