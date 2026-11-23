# Knowledge bases

A knowledge base is a folder of short Markdown notes, called concepts, that an agent can look up, search and add to. Each concept is one file with a few fields at the top (a type, a title, tags and so on) and text below. Knowledge bases use the [Open Knowledge Format (OKF) v0.3](https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81aff55992be21507e97004286789126f88c/SPEC.md), so a knowledge base written here can be read by any OKF tool, and an OKF bundle made elsewhere can be given to an agent.

## Turn them on

On the agent's card on the **Agents** page, turn on **Allow knowledge bases**. The agent then gets:

- knowledge bases of its own, which it creates simply by writing to a new name;
- the shared knowledge bases you list below the switch;
- the three tools `kb_get`, `kb_search` and `kb_put` (below).

With the switch off the agent has none of these. **Denied tools** still removes a single tool, for example `kb_put` to make every knowledge base read-only for that agent.

## Own and shared knowledge bases

**Own knowledge bases** are folders in the agent's workspace, under `okf/`: `okf/notes/` is the knowledge base `notes`. The agent can always read and write them, and creates one by writing its first concept to a new name. Names are one folder name: no `/`, and not starting with a dot.

**Shared knowledge bases** are folders anywhere on the machine that you give the agent under **Shared knowledge bases** on its card:

| Column | Meaning |
|---|---|
| **Name** | How the agent names it, after `shared/`. Letters, digits, `-`, `_` and `.`, not starting with a dot; each name once per agent. |
| **Folder** | The folder's absolute path, for example `/opt/okf/pentesting`. |
| **Allow writing** | Off: the agent may only read and search it. On: it may also add and change concepts. |

The agent names a shared knowledge base with the `shared/` prefix, for example `shared/pentesting`, so its own `pentesting` and `shared/pentesting` are two different knowledge bases. The agent cannot create a shared knowledge base; writing to one that is read-only or not listed is refused with a message naming it.

A duplicate or invalid name and a folder that is not an absolute path are refused when you save, for example "Alice has more than one shared knowledge base named "pentesting"." A shared folder that does not exist is marked **Folder not found** on its row, and Check Up lists it; the agent simply does not have that knowledge base until the folder is there. While the switch is off the list says "Not used: knowledge bases are off for this agent."

New files in a shared folder get the folder's own permissions (without execute), so a folder shared with a group stays shared; a changed file keeps its permissions. Files in the agent's own knowledge bases get the same permissions as the rest of its workspace.

In `config.json`:

```json
{
  "id": "alice",
  "knowledge": {
    "enabled": true,
    "shared": [
      { "name": "pentesting", "path": "/opt/okf/pentesting", "write": false }
    ]
  }
}
```

## What the agent sees

The agent's instructions list its knowledge bases, its own first, then the shared ones, read-only ones marked:

```
Bundles: notes, shared/pentesting (read only)

Note: Bundle names may conflict. Do not assume that "mybundle" and "shared/mybundle" are the same.
```

An agent that has none yet reads `Bundles: none yet`.

The file tools cannot read or write `okf/`: the agent reaches its knowledge bases only through the tools below. A [clone or sub-agent](./subagents.md) of the agent uses the same knowledge bases as the agent.

## The tools

- `kb_get`: one concept by its ID, its path in the knowledge base without `.md` (`guidance/crypto-review`), with its fields, its text and the concepts it includes.
- `kb_search`: the best-matching sections of one knowledge base for some words, each with the concept ID, title, heading and a short passage.
- `kb_put`: creates or replaces one concept. The whole concept is replaced, so the agent gets it, changes it and puts it back; `type` is required.

Edits made to the files outside ClawEh, by hand or with Git, are seen at the next search.
