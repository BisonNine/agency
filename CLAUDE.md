# Agency — project instructions for Claude Code

Agency (`go.bison9.com/agency`, binary `agency`, `main.go` at the root) is an
independent Go project forked from sipeed/picoclaw on 2026-03-20; it is not a
picoclaw fork for upstream PRs. One process serves the agent runtime, the
WebUI, the API and the MCP host. Detailed architecture and invariants:
**`docs/architecture.md`** — read the section for the area you touch.

Keep this file short and current: when a change makes a passage wrong, fix it
in the same change. Detail belongs in `docs/architecture.md`, not here.

## Status: released

Config schemas, tool names, API shapes and the device protocol are depended on.

- **Ask first** before any breaking change (renaming a config key or tool,
  changing a JSON shape, dropping a flag or endpoint); say what would break.
- **Ask first** before adding compatibility code (old keys, aliases, old
  protocol versions). Never add it on your own initiative.
- Version lives in `app/app.go`. Bumping it is a normal change; tagging is not.

## Changelog

`CHANGELOG.md` (Keep a Changelog) is updated **in the same commit** as any
change an operator or integrator can see: config keys and defaults, tool names
and arguments, HTTP/MCP/device-protocol shapes, CLI commands and flags,
security behaviour, user-visible messages. Not for refactors, tests or
internal changes.

- Add to the topmost `## [x.y.z]` (the version in `app/app.go`). No
  `[Unreleased]` section.
- Headings in order, empty ones omitted: Security, Added, Changed, Deprecated,
  Removed, Fixed.
- Prefix install-breaking entries with **BREAKING** and give the migration.
- Write for someone who has not seen the code: name the key or tool.
- If the topmost version has already been released, say so and ask for the
  version bump first.

## Product name

The product name lives only in `app/app.go`; a rename must be an edit there
only. Use `app.Name()` (identity), `app.ShortName()` (prose), `app.BinaryName()`
(what to type). Fixed on-disk/protocol identifiers are constants in
`global/identifiers.go`, never derived from the name. Two guards in `make test`
fail on the old product name anywhere in the tree. Details: architecture doc,
"Product name".

## Invariants (each has a guard test)

- `config` imports no SDK package but `modelspec`.
- No SDK package (`go.bison9.com/agent/...`) imports Agency; `cogmem` and
  `ctxengine` never do.
- `forum` imports only `forum/forumfs`; `classifier` only `classifierfs`,
  `internal/mergepatch`, `internal/dotpath`; `ccv3` is stdlib plus mergepatch.
- WebUI handlers read and write config only through `config.Store`
  (`h.currentConfig()`, `h.updateConfig(fn)`), never `config.LoadConfig`.
- Agent ids are compared with `config.SameAgentID`, never `==` or `EqualFold`.
- Task-model lists are read only through `TaskModelsConfig.Entries(use)`.
- Paths inside the data directory are stored relative to it
  (`config.ToStoredPath` / `config.ResolvePath`).
- A new alert gets a row in `ALERTS.md`; alerts stay `Normal` priority.
- First-party libraries (agent SDK, Maestro, MCPFusion, vulncheck) are fixed in
  the library, never worked around here.

## Build and test

```
make build              # binary with embedded frontend and manual
make test               # the full gate (lint, govulncheck, Go -race, frontend, MCP integration)
go test ./...           # fast Go-only loop
make check-webui        # browser suite against a running dev instance
make test-deb           # Debian package in containers (Docker/Podman)
make test-maestro-host  # Maestro MCP suite against a live Agency
```

- Frontend changes: `cd web/frontend && pnpm run build:backend`.
- `make test` must pass before anything is called done. See `docs/test.md`.
- New behaviour needs tests for both the on and off paths. On a rename, grep
  the whole repo (`*_test.go`, `test.sh`, `tests/`, docs) for the old name.
- Every new tool gets a probe case in `tests/test_mcpserver.sh`.
- Test gateways source `tests/lib/gateway_env.sh` so they never alert the operator.
- **Capability changes** (tool gating, allowlists, suite switches, CLI args or
  env, their defaults) change `tests/capabilities/testdata/effective.golden`.
  Regenerate (`UPDATE_GOLDEN=1 go test ./tests/capabilities/`), read the diff,
  and add a **BREAKING** changelog entry naming how to restore access. Such a
  default must also show on Check Up and in the reply when it bites.
- **WebUI suite**: run `make check-webui` for any significant change to
  `web/frontend`, the `/api/*` handlers, startup, readiness or reload. The plan
  is `docs/webui-test-plan.md`, executed by `tests/frontend-e2e.mjs`; keep them
  in step. Wait for `/ready`, not `/health`. When a step fails, decide whether
  the plan or the product is wrong. Dev instance only. If unsure whether a run
  is needed, ask.
- Releases are cut by the maintainer with b9; `make` must work without it.
  Vulnerabilities without a fix go under `govulncheck.ignore` in `b9.yaml` with
  a short `until`; never weaken the scan another way.

## Workflow

- During the refactor (`refactor/refactor.md`) all work lands on the
  `refactor` branch. Start worktrees from its tip, land tested work back as
  soon as it passes, remove the worktree. Commit and push at logical points.
- `main` stays empty until the refactor is done. Never push to it.
- Never create, move or delete git tags.
- When investigating a problem, report findings and wait for approval.
- Never build to, install to or restart a production instance; test on dev
  (machine details in `CLAUDE.local.md`).
- **Finishing:** the committed work must be at the tip of the current branch
  in `~/source/b9/agency` (always written with `~/`) and built there. Say the
  branch and commit and that it is ready to test.
- Use Alice and Bob as example agent names.
