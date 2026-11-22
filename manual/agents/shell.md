# Shell commands and jobs

An agent granted `shell_exec` can run commands on this machine: git, builds and tests, and Unix tools such as grep, sed and jq. It can wait for a command, or start it as a **job** that keeps running while the agent does other work, and come back to it with `shell_ctrl`.

Granting it is covered in [Tools and skills](./tools-and-skills.md#shell-commands). Commands run with the full access of the account the service runs as; the deny patterns still block dangerous ones.

## The two tools

| Tool | What it does |
|---|---|
| `shell_exec` | Runs a command and returns its output, or starts it as a job and returns at once. |
| `shell_ctrl` | Lists the agent's jobs, waits on one, sends it input, or stops it. |

`shell_ctrl` comes with `shell_exec`: an agent granted `shell_exec` has both, and an agent without it has neither. It is not a box of its own on the **Agents** page. To withhold it, add `shell_ctrl` to the agent's **Denied tools** (`deny_tools`); the agent then keeps `shell_exec` alone and works as it did before jobs existed (see [Timeout](#timeout)).

## Running a command

Alice asks for a build and gets the output when the command ends. Arguments of `shell_exec`:

| Argument | Meaning |
|---|---|
| `command` | The command, run by `sh`. |
| `working_dir` | Where to run it; a relative path starts at the agent's workspace. Without it, the command runs where the agent's previous command in the same conversation ended (after a `cd`), else in the workspace. |
| `timeout_seconds` | How long to wait for the command. Default `tools.exec.timeout_seconds` (60), at most `tools.exec.max_timeout_seconds` (600). |
| `background` | Start the command as a job and return at once. |
| `notify` | For a job: tell the agent when it ends. On by default. |

Long output is returned as its beginning and its end, with the middle left out, and the result names the file that holds all of it. Standard output and standard error are in the order they were written.

### Timeout

A command still running at its timeout is **not stopped**: it is kept as a job. The result gives the output so far, the job's id and its output file, and the agent can wait on it or stop it with `shell_ctrl`. It is told when the job ends.

An agent without `shell_ctrl` (in **Denied tools**) cannot collect a job, so for it the command is stopped at its timeout, with everything it started.

## Jobs

Bob asks Alice to run the test suite in the background and carry on reviewing. Alice starts it with `background`, gets its id and output file at once, and later:

- **`list`**: her jobs, each with its state (`running`, `exited`, `stopped` or `lost`), exit code, start and end, output size and output file.
- **`wait`**: blocks until the job ends, a new line of its output matches `until` (a regular expression), or `seconds` pass (default 30), and returns the output since her last wait.
- **`input`**: writes `text` to the job's standard input, with a newline unless `raw`. Only a job started with `background` takes input.
- **`stop`**: ends the job and everything it started: asked to stop, then killed after 5 seconds. `all` stops all her running jobs.

When a job ends, Alice is told in her conversation with the command, how it ended, how long it ran, its last lines and its output file. Her answer goes to the chat she started the job from, as for a [sub-agent](./subagents.md) running in the background. Stopping a job with `shell_ctrl` sends no notice.

A job belongs to the agent that started it. A [sub-agent clone](./subagents.md) of Alice acts as Alice: it sees and stops her jobs. Bob cannot see or stop them.

Jobs are not tied to a turn: `/cancel` and the end of a turn leave them running. They are stopped when the service stops, without a notice. A job that was running when the service went down (a crash or a restart) is marked `lost` when it starts again, and the agent is told once; it is never started again.

[Classifiers](./classifiers.md) and [watch jobs](./scheduling.md) cannot start jobs or use `shell_ctrl`: nothing would collect the result. Their commands run in the foreground and are stopped at the timeout.

## Output files

Each job's output is written to `state/shell/<id>.log` in the agent's workspace as it is produced, with a small record beside it. The agent reads and searches the log with `file_read_lines` and `file_search_lines`; the file tools may read this folder but never write it. Past `tools.exec.max_job_output_bytes` the middle of the log is dropped and the job keeps running.

A command that ran in the foreground and whose whole output was returned leaves no file. Finished jobs' files are removed after `tools.exec.job_log_days`. The folder is not in the [backup](../admin/backup.md).

## Settings

Under `tools` › `exec` in `config.json`. Leaving a key out, or setting it to 0, uses the default; a negative value is refused. A change applies from the next start.

| Key | Default | Meaning |
|---|---|---|
| `timeout_seconds` | 60 | How long a command runs in the foreground before it is kept as a job. A value above `max_timeout_seconds` is used as `max_timeout_seconds`. |
| `max_timeout_seconds` | 600 | The most `timeout_seconds` an agent may ask for, and the longest `wait`. |
| `max_jobs` | 8 | Running jobs per agent. A start beyond it is refused, naming the running jobs. |
| `max_job_seconds` | 86400 | A job is stopped after this long. |
| `max_job_output_bytes` | 10485760 (10 MiB) | The most output kept per job. |
| `job_log_days` | 7 | Days a finished job's files are kept. |

The deny patterns (`enable_deny_patterns`, `custom_deny_patterns`) and `custom_allow_patterns` screen every command, foreground or job. Input sent to a job is not screened.

## Where to see it

- [Check Up](../admin/checkup.md) shows `shell_ctrl` in each agent's tools "with shell_exec", a **Shell jobs (shell_ctrl)** row with the agent's limits, and the **Shell job logs** folder under Data.
- The [audit log](../admin/audit.md) has a row for every start (command, folder, background or not), every input (its length, never its content) and every stop.
