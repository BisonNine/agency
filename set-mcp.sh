#!/bin/sh
#
# set-mcp.sh — register (or refresh) the Agency MCP server in whichever local AI
# CLIs are installed: Codex CLI (codex), Claude Code (claude), Antigravity CLI
# (agy), OpenCode (opencode), and Cursor CLI (cursor-agent). Run it after
# installing/updating any of those CLIs, or after changing the Agency MCP port.
# CLIs that are not on your PATH are skipped.
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

# Each CLI is refreshed one at a time (register, then list) and only when its
# binary is on the PATH. Everything is written to the user-level (global) config.

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
    claude mcp remove agency 2>/dev/null || true
    claude mcp add --transport http agency --scope user "$AGENCY_MCP_URL"
    claude mcp list
    echo ""
else
    echo "== Claude Code: 'claude' not on PATH — skipping =="
    echo ""
fi

if have agy; then
    echo "== Antigravity CLI =="
    # 'add' updates an existing entry in place, so no remove is needed.
    # Flags must come before the server name.
    agy mcp add --type http agency "$AGENCY_MCP_URL"
    agy mcp list
    echo ""
else
    echo "== Antigravity CLI: 'agy' not on PATH — skipping =="
    echo ""
fi

if have opencode; then
    echo "== OpenCode =="
    # There is no 'mcp remove'; 'add' overwrites an existing entry.
    opencode mcp add --global --url "$AGENCY_MCP_URL" agency 
    # The background service connects asynchronously after picking up the new
    # config, so 'list' reports nothing for a moment. Wait for it to settle.
    opencode reload >/dev/null 2>&1 || true
    i=0
    while [ "$i" -lt 10 ]; do
        out=$(opencode mcp list 2>&1)
        case "$out" in
            *agency*) break ;;
        esac
        i=$((i + 1))
        sleep 1
    done
    echo "$out"
    echo ""
else
    echo "== OpenCode: 'opencode' not on PATH — skipping =="
    echo ""
fi

# Cursor's CLI is installed as 'cursor-agent' (with an 'agent' alias). It has no
# 'mcp add'; servers are read from ~/.cursor/mcp.json, so merge the entry there
# (keeping any other servers) and then approve it.
if have cursor-agent; then
    echo "== Cursor CLI =="
    CURSOR_MCP_JSON="$HOME/.cursor/mcp.json"
    mkdir -p "$HOME/.cursor"
    tmp="$CURSOR_MCP_JSON.tmp.$$"
    ok=1
    if [ ! -s "$CURSOR_MCP_JSON" ]; then
        printf '{\n  "mcpServers": {\n    "agency": {\n      "url": "%s"\n    }\n  }\n}\n' \
            "$AGENCY_MCP_URL" > "$tmp" || ok=0
    elif have jq; then
        jq --arg url "$AGENCY_MCP_URL" '.mcpServers.agency = {url: $url}' \
            "$CURSOR_MCP_JSON" > "$tmp" || ok=0
    elif have python3; then
        python3 - "$CURSOR_MCP_JSON" "$AGENCY_MCP_URL" > "$tmp" <<'PY' || ok=0
import json, sys
with open(sys.argv[1]) as f:
    cfg = json.load(f)
cfg.setdefault("mcpServers", {})["agency"] = {"url": sys.argv[2]}
json.dump(cfg, sys.stdout, indent=2)
sys.stdout.write("\n")
PY
    else
        echo "Need jq or python3 to update the existing $CURSOR_MCP_JSON" >&2
        ok=0
    fi
    if [ "$ok" -eq 1 ]; then
        mv "$tmp" "$CURSOR_MCP_JSON"
        echo "Wrote agency to $CURSOR_MCP_JSON"
        cursor-agent mcp enable agency 
        cursor-agent mcp list
    else
        rm -f "$tmp"
        echo "Could not update $CURSOR_MCP_JSON — left unchanged" >&2
    fi
    echo ""
else
    echo "== Cursor CLI: 'cursor-agent' not on PATH — skipping =="
    echo ""
fi

echo "Done."
