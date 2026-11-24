#!/bin/sh
#
# set-mcp.sh — register (or refresh) the Agency MCP server in whichever local AI
# CLIs are installed: Antigravity (agy), Codex CLI (codex), Claude Code (claude)
# and OpenCode (opencode). Run it after installing/updating any of those CLIs, or
# after changing the Agency MCP port. CLIs that are not on your PATH are skipped.
#
# The WebUI does the same from each CLI's row on the Providers page (Register
# tool server); the commands here are the ones it runs (internal/mcpreg), so
# keep the two in step.
#
# Cursor is deliberately not handled here: it has no `mcp add` subcommand and is
# configured by editing ~/.cursor/mcp.json, so this script reports what to add
# rather than rewriting a file it does not own.
#
# ENDPOINT — Agency exposes two MCP endpoints (see docs/mcp.md):
#     /mcp       standard bearer auth (Authorization: Bearer <token>)
#     /internal  session_token parameter — Agency's multi-assistant routing
#   A local CLI that authenticates as MULTIPLE agents must use /internal: it
#   supplies a per-agent session_token on each call, so one CLI install can act
#   as several agents. That is what this script configures.
#
# PORT — the URL below must match the MCP host `listen` port in your Agency config
#   (tools → mcp_host → listen, e.g. 127.0.0.1:5911). If you change that port,
#   update AGENCY_MCP_URL here, or export it before running:
#       AGENCY_MCP_URL=http://127.0.0.1:6000/internal ./set-mcp.sh

AGENCY_MCP_URL="${AGENCY_MCP_URL:-http://127.0.0.1:5911/internal}"

set -u

have() { command -v "$1" >/dev/null 2>&1; }

echo "Agency MCP endpoint: $AGENCY_MCP_URL"
echo ""

# Each CLI is refreshed one at a time and only when its binary is on the PATH.
# Claude and Codex refuse or keep an existing entry, so it is removed first;
# Antigravity's add updates one in place.

if have agy; then
    echo "== Antigravity =="
    agy mcp add --type http agency "$AGENCY_MCP_URL"
    agy mcp list
    echo ""
else
    echo "== Antigravity: 'agy' not on PATH — skipping =="
    echo ""
fi

if have codex; then
    echo "== Codex CLI =="
    codex mcp remove agency 2>/dev/null || true
    codex mcp add agency --url "$AGENCY_MCP_URL"
    codex mcp list
    echo ""
else
    echo "== Codex CLI: 'codex' not on PATH — skipping =="
    echo ""
fi

if have claude; then
    echo "== Claude Code =="
    claude mcp remove agency --scope user 2>/dev/null || true
    claude mcp add --transport http agency --scope user "$AGENCY_MCP_URL"
    claude mcp list
    echo ""
else
    echo "== Claude Code: 'claude' not on PATH — skipping =="
    echo ""
fi

if have opencode; then
    echo "== OpenCode =="
    # `opencode mcp add` replaces the whole entry and has no flag for Code Mode,
    # so it is turned off again in the file the command wrote: with it on, the
    # model must write a script to call a tool, and weaker models fail.
    if out=$(opencode mcp add agency --url "$AGENCY_MCP_URL" --global); then
        echo "$out"
        cfg=$(printf '%s\n' "$out" | sed -n 's/^MCP server "agency" added to //p')
        if [ -n "$cfg" ] && have python3 && python3 - "$cfg" 2>/dev/null <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    config = json.load(f)
config["mcp"]["servers"]["agency"]["codemode"] = False
with open(path, "w") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
PY
        then
            echo "Code Mode off for agency in $cfg"
        else
            echo "Turn Code Mode off by hand: add \"codemode\": false to mcp.servers.agency in the global opencode.json"
        fi
    fi
    echo ""
else
    echo "== OpenCode: 'opencode' not on PATH — skipping =="
    echo ""
fi

echo "Done."

if have cursor-agent || have agent; then
    echo "== Cursor: no 'mcp add' subcommand — configure it by hand =="
    echo "   Add this to ~/.cursor/mcp.json under \"mcpServers\":"
    echo "     \"agency\": { \"url\": \"$AGENCY_MCP_URL\" }"
    echo ""
fi
