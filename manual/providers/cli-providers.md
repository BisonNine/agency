# CLI providers

This page shows how to run agents on a coding CLI installed on the same machine: Claude Code, Codex, Antigravity, Cursor or OpenCode.

Instead of calling an API, Agency starts the CLI program for each model call, hands it the conversation, and reads back its answer. The CLI signs in with its own account (often a subscription), runs its own tool loop, and reaches Agency's tools through Agency's MCP server.

| CLI | Protocol | Program | Permission-bypass flag |
|---|---|---|---|
| Claude CLI (Claude Code) | `claude-cli` | `claude` | `--dangerously-skip-permissions` |
| Codex CLI | `codex-cli` | `codex` | `--dangerously-bypass-approvals-and-sandbox` |
| Antigravity CLI | `antigravity-cli` | `agy` | `--dangerously-skip-permissions` |
| Cursor CLI | `cursor-cli` | `cursor-agent` | `--yolo` |
| OpenCode CLI | `opencode-cli` | `opencode` | `--auto` |

Antigravity replaces the Gemini CLI, which Google deprecated. A configuration that still names the protocol `gemini-cli` keeps working and runs `agy`.

## Install and sign in

1. Install the CLI on the Agency machine, following the vendor's instructions.
2. Sign in once, **as the user the Agency service runs as** (for OpenCode, `opencode auth login`). The CLI keeps its login in that user's home directory, and Agency starts it as that user. A login made under your own account does not help a service running as another user.
3. Check that the service can find the program. Agency looks for it on the `PATH` the service runs with, which `claw install` records when it installs the service. If you install a CLI later, or its location changes (for example after a Node.js or nvm upgrade), run `claw install` again, or set the program's full path in **Command** on the CLI's provider.

A CLI can also authenticate with an API key from the service's environment. Agency passes the CLIs every variable starting with `ANTHROPIC_`, `OPENAI_`, `GOOGLE_`, `GEMINI_`, `CLAUDE_`, `CODEX_`, `CURSOR_`, `AGY_`, `ANTIGRAVITY_` or `OPENCODE_`, on top of the basic variables such as `PATH` and `HOME`. Other variables of the service are not passed.

## Switch a CLI on

Open **Providers**. The **Local CLI agents** section lists every supported CLI:

- A CLI whose program was found shows its path and a switch. One that was not found is greyed out with "`<program>` not found on PATH", and its switch is disabled.
- The **Args** line shows the full command line Agency runs.

Turning the switch on creates whatever is missing: a provider for the CLI and a model named after it, enabled, with a request timeout of 3600 seconds. Turning it off disables every model that runs through that CLI. Nothing is deleted, so switching it back on restores what was there.

Then give the model to an agent on the **Agents** page, or click **Set as default** on its card on the **Models** page.

### Choosing the CLI's model

The **Model ID** decides which model the CLI uses:

- The protocol name (`claude-cli`, `codex-cli`, `antigravity-cli`, `cursor-cli`, `opencode-cli`) lets the CLI use its own default model. This is what the switch creates.
- Anything else is passed to the CLI as its model (`--model`, or `-m` for Codex). The catalogue has **Claude CLI Opus** (`Opus`) and **Claude CLI Fable** (`Fable`).

To add another, click **Add Model** on the Models page, pick the CLI's provider, and enter the CLI's own name for the model.

## Allow CLI to bypass restrictions

Once a CLI is set up, its row shows a checkbox, **Allow CLI to bypass restrictions** (`bypass_restrictions` on the provider). It is off by default and applies to every model of that CLI.

**Off:** Agency does not pass the bypass flag. The CLI follows its own permission settings, and a tool call it would have to ask about is refused, because nobody is there to approve it. When that happens Agency does not pass on an empty or apologetic answer; the person gets a reply naming the CLI and the refused tools, for example:

```
Alice: The Claude CLI declined to use tools (file_write). Tick *Allow CLI to bypass restrictions* for this CLI on the Providers page, or allow the tools in the CLI's own settings.
```

The reply starts with the agent's name, so it is clear whose CLI refused in a chat shared by several agents. Agency does not try the agent's next model in this case.

To keep bypass off and still let the CLI use Agency's tools, allow those tools in the CLI's own permission settings for the service user. Agency's tools reach the CLI under the MCP server name `claw` (tool names start with `mcp__agency__`, or `claw_` in OpenCode). See the CLI's documentation for how to allow them.

**On:** Agency passes the flag from the table above on every call. The CLI can then run commands and change files anywhere the service user can, without asking, outside Agency's own workspace and shell controls. Turn it on only for a CLI running as a user whose access you are comfortable giving to the agent.

For OpenCode, unticking the box only removes `--auto`; OpenCode's own configuration decides what its built-in tools may do.

Either way, the [Check Up](../admin/checkup.md) page has one row per CLI provider saying which state it is in.

## Extra arguments

A CLI model's `extra_args` in `config.json` are added after the arguments Agency always passes. They are not on the WebUI form. Agency supplies the arguments each CLI needs to run unattended (for example `--no-chrome` for Claude and `--skip-git-repo-check` for Codex), so a model does not need them, and one given again is not passed twice.

A bypass flag written into `extra_args` is removed while **Allow CLI to bypass restrictions** is off, so the checkbox stays the one place this is decided. Agency logs a warning, and the agent's card on the **Agents** page shows "`<provider>` is not allowed to bypass its restrictions." with an **Allow it** link.

A model's `env` (also `config.json` only) sets extra environment variables for that model's CLI. The Claude CLI always gets `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, which turns off Claude Code's own memory so it does not compete with the agent's.

## Where the CLI runs

The CLI runs in the model's **Workspace Path** (`workspace`). When that is empty, it runs in `<AGENCY_HOME>/cli`, shared by all CLI models. The **Check Up** report lists each CLI model's command line and working directory.

Temporary agents created fresh at run time (see [Sub-agents](../agents/subagents.md)) are the exception: their CLI runs in the agent's own empty folder and never with the bypass flag, whatever the checkbox says.

## Timeouts

A CLI runs a whole job in one call, so CLI models are created with **Request Timeout (s)** 3600. Each turn is still limited by **Turn timeout (s)** on the System page, 900 seconds by default, which stops the turn and its CLI even if the request timeout is longer. Raise the turn timeout if your CLI jobs need more than 15 minutes. See [Concepts](./concepts.md#request-timeout).

## Tools: Agency's MCP server

A CLI model does not receive Agency's tools in its prompt. It calls them through Agency's MCP server, which listens on `127.0.0.1:5911` by default and starts automatically while any enabled model uses a CLI. Its settings are under **MCP** → **Config** (**Enabled**, **Auto-enable**, **Listen Address**).

Each CLI has to be told about this server once, as the service user, under the name `claw` and with the `/internal` endpoint:

```bash
# Claude Code
claude mcp add --transport http claw --scope user http://127.0.0.1:5911/internal

# Codex CLI
codex mcp add claw --url http://127.0.0.1:5911/internal

# Antigravity
agy mcp add claw http://127.0.0.1:5911/internal --type http
```

Cursor has no such command. Add this to `~/.cursor/mcp.json` of the service user, under `"mcpServers"`:

```json
"claw": { "url": "http://127.0.0.1:5911/internal" }
```

OpenCode:

```bash
opencode mcp add claw --url http://127.0.0.1:5911/internal --global
```

The command has no option for Code Mode, so turn it off by hand: add `"codemode": false` to the `claw` entry in the service user's `~/.config/opencode/opencode.json`. With Code Mode on, the model must write a script to call a tool, and weaker models fail. The entry then reads:

```json
{"mcp":{"servers":{"claw":{"type":"remote","url":"http://127.0.0.1:5911/internal","codemode":false}}}}
```

If OpenCode has no web search provider set up, also add `"permission":{"websearch":"deny"}` to that file: otherwise a run that tries its built-in web search fails.

`./set-mcp.sh` in the Agency source folder runs these commands for every CLI that is installed, and turns off Code Mode for OpenCode.

If you change **Listen Address**, use the new port in these commands. [MCP](../mcp.md) covers the server in full. Without this step the CLI still answers, but it has none of Agency's tools.

Agency puts a session token in each agent's prompt, and the CLI sends it with every tool call. That is how one CLI installation can serve several agents: each call runs as the agent whose token it carries, with that agent's [tools](../agents/tools-and-skills.md), folders and permissions. The tools of external MCP servers that an agent is allowed reach the CLI the same way, through Agency.

While another agent's request runs on the same agent, a CLI agent's chat output for that moment is refused rather than risk the wrong chat.

## Related

- [Concepts](./concepts.md) for fallback chains and cooldowns, which apply to CLI models too: put an API model after a CLI model in an agent's list, and the agent still answers when the CLI fails.
