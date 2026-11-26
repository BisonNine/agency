# Command line

This page lists every `agency` command and its main options.

Run the commands on the machine Agency is installed on, as the user it runs as. On a system install that is, for example, `sudo -u <service user> AGENCY_HOME=/opt/agency agency status`.

`agency <command> --help` shows the full help for any command.

## Starting Agency

| Command | What it does |
|---|---|
| `agency` | Starts Agency in the foreground: the agents, the channels and the WebUI. This is what the service runs. |
| `agency gateway` | The same as `agency`. Alias: `agency g`. |

Options:

- `-d`, `--debug` — log at debug level.
- `-T`, `--no-truncate` — do not shorten long values in debug logs. Only together with `--debug`.

## Installing and upgrading

| Command | What it does |
|---|---|
| `agency install` | Copies the binary, registers the background service, creates the admin account if there is none, and starts Agency. Run with `sudo` for a system service. |
| `agency uninstall` | Stops and removes the service. The binary and the data directory are kept. |
| `agency upgrade` | Downloads the latest release, verifies its signature, replaces the binary and restarts the service. Alias: `agency update`. |
| `agency version` | Shows the version. Alias: `agency v`. |

`agency install` options:

- `--allowed-cidrs <list>` — networks allowed to reach the WebUI: CIDRs separated by commas, `private`, or `any`. Empty means this machine only.
- `--host <address>` — where plain HTTP listens. Default `127.0.0.1`. `0.0.0.0` serves unencrypted HTTP on the network and needs `--allowed-cidrs`.
- `--port <n>` — the plain-HTTP port.
- `--bin-dir <dir>` — where to put the binary.
- `--user <name>` — macOS only: the account a system service runs as. Default: the user who ran `sudo`. On Linux a system service runs as `agency`.
- `-y`, `--yes` — skip the confirmation question. The admin account is still required.

`agency uninstall` options: `--user`, `-y`.

`agency upgrade` options:

- `--check` — only report whether a newer release exists.
- `--version <tag>` — install that release instead of the latest.
- `--force` — reinstall even if that version is already running.
- `-y`, `--yes` — skip the confirmation question.

See [Installing Agency](../getting-started/install.md) and [Upgrading](../getting-started/upgrading.md).

## Status and access

| Command | What it does |
|---|---|
| `agency status` | Shows whether Agency is running, the WebUI addresses, the certificate, the admin account, and the providers. Alias: `agency s`. |
| `agency admin [username]` | Creates or replaces the WebUI admin account. Asks for the password twice. Also clears every login lockout. |
| `agency network [cidrs]` | Shows or sets who may reach the WebUI. |
| `agency tls` | Shows the HTTPS certificate: source, names, expiry and fingerprint. |

`agency network`:

- With no argument it allows the private LAN ranges. Otherwise give CIDRs separated by commas, or `private`, `any` or `none` (this machine only).
- `--show` — print the current settings and change nothing.
- `--http localhost|network` — where plain HTTP listens.
- `--https all|localhost|off` — where HTTPS listens.
- `--device localhost|network` — where the device listener listens.

A new allowlist applies within seconds, without a restart. The listener options apply after a restart.

`agency tls --regenerate` replaces the self-signed certificate now.

See [Signing in](../admin/login.md), [Network access](../admin/network.md) and [HTTPS and certificates](../admin/https.md).

## Models

| Command | What it does |
|---|---|
| `agency model` | Shows the default model and the models that are enabled and have credentials. |
| `agency model <name>` | Makes `<name>` the default model. The model must exist and be enabled. |
| `agency test models` | Sends a short prompt to every enabled model and reports which answer. |
| `agency test model <name>` | Tests one model. |

`agency test` options: `-t`, `--timeout <seconds>` per model (default 30).

## Talking to an agent

| Command | What it does |
|---|---|
| `agency agent` | Starts an interactive chat with the default agent in the terminal. |
| `agency agent -m "<message>"` | Sends one message and prints the reply. |

`--model <name>` uses another model for this run.

## Scheduled jobs

| Command | What it does |
|---|---|
| `agency cron list` | Lists the scheduled jobs. |
| `agency cron add` | Adds a job. |
| `agency cron enable <id>` | Enables a job. |
| `agency cron disable <id>` | Disables a job. |
| `agency cron remove <id>` | Removes a job. |

Alias: `agency c`.

`agency cron add` options:

- `-n`, `--name` (required) — the job name.
- `-m`, `--message` (required) — the message sent when the job runs.
- `-c`, `--cron '<expression>'` — a cron schedule, for example `'0 9 * * *'`; or
- `-e`, `--every <seconds>` — run every N seconds.
- `--channel` and `--to` — the channel and chat the message is delivered on; the agent bound there handles it.
- `--peer-kind channel|direct` — `direct` for a direct-message target. Default `channel`.

## Skills

| Command | What it does |
|---|---|
| `agency skills list` | Lists installed skills. |
| `agency skills show <name>` | Shows a skill. |
| `agency skills search [query]` | Searches available skills. |
| `agency skills install <owner/repo/path>` | Installs a skill from GitHub into the shared skills folder. `--registry <name> <slug>` installs from a registry instead. |
| `agency skills remove <name>` | Removes a skill. Aliases: `rm`, `uninstall`. |

An installed skill is available to an agent only when the agent's skill list includes it.

## Devices

| Command | What it does |
|---|---|
| `agency devices pair` | Sets up the device listener and prints a pairing QR code. |
| `agency devices list` | Lists pending and paired devices. |
| `agency devices approve <request-id>` | Approves a pending pairing. |
| `agency devices reject <request-id>` | Rejects a pending pairing. |
| `agency devices remove <device-id>` | Removes a paired device and revokes its tokens. |
| `agency acp` | Started by the Rabbit R1's agent, not by you. It bridges the R1 to the running Agency. `--no-auto-pair` requires a manual approval; `--url` sets the device listener address. |

## Service tokens

A service token lets an external MCP client use an agent's tools.

| Command | What it does |
|---|---|
| `agency token issue <agent>` | Creates or replaces the agent's token and prints it. |
| `agency token rotate <agent>` | The same as `issue`. |
| `agency token revoke <agent>` | Removes the agent's token. |
| `agency token list` | Lists the agents that have a token. Tokens are not shown. |

A running Agency picks up changes within a few seconds.

## Backup and restore

| Command | What it does |
|---|---|
| `agency backup` | Writes a `agency-backup-<timestamp>.tar.gz` archive. Safe while Agency runs. `--dest <dir>` writes it elsewhere. |
| `agency restore <archive>` | Restores an archive. Agency must be stopped. Files it replaces are kept in `restore-backup-<timestamp>/`. `--yes` skips the confirmation. |

See [Backup and restore](../admin/backup.md).

## Maintenance

| Command | What it does |
|---|---|
| `agency memory purge` | Reports what in every agent's memory is no longer active: archived domains and retired or superseded memories. Add `--confirm` to delete it. Stop Agency first. |
| `agency sessions erase --channel <name> --chat <id>` | Deletes the old per-sender conversations of one sender. `--all` also deletes the agents' shared main conversation. Agency must be stopped. |
| `agency sessions migrate` | Converts conversation files from older releases. Run once, with Agency stopped. |

## Shell completion

`agency completion bash|zsh|fish|powershell` prints a completion script for your shell.

## Related pages

- [Configuration file](./configuration.md)
- [Troubleshooting](./troubleshooting.md)
