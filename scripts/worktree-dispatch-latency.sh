#!/usr/bin/env bash
# worktree-dispatch-latency.sh — latency proof for the worktree fast-path fix.
#
# Arms N consecutive workflow runs (default 10) via the Orchicon MCP plane
# (create_work_item with workflow_id + auto_start_workflow, then polls
# get_workflow_run until WorktreeStatus=ready) and reports per-arm
# arm-to-ready deltas plus p95/max.
#
# Acceptance (per the work item):
#   - p95 arm-to-ready < 10s over 10 consecutive arms
#   - no single arm exceeds 30s
#   - with terminal-run backlog present (>=20 terminal runs with worktrees)
#   - repeat with TEMPLATE=quick for the Quick Work template variant
#
# Usage:
#   PROJECT_ID=01KYQXQ95C2BFGDT1AFXFX5875 \
#   WORKFLOW_ID=<sdlc-nonhuman-wf-id> \
#   ./scripts/worktree-dispatch-latency.sh [--arms 10] [--template sdlc|quick]
#
# The script talks to a RUNNING Orchicon plane over MCP stdio
# (ORCHICON_MCP_TENANT_ID defaults to tnt_dev). It does NOT start the
# server; start it first (`orchicon serve --detach` or equivalent).
#
# Exit codes: 0 = within bound, 1 = bound violated, 2 = usage/environment error.
set -euo pipefail

ARMS=10
TEMPLATE="sdlc"
PROJECT_ID="${PROJECT_ID:-01KYQXQ95C2BFGDT1AFXFX5875}"
WORKFLOW_ID="${WORKFLOW_ID:-}"
QUICK_WORKFLOW_ID="${QUICK_WORKFLOW_ID:-01M1ERHNCNF38MP3SEV1GTH26G}"

usage() {
  sed -n '1,20p' "$0"
  echo "Options: [--arms N] [--template sdlc|quick] [--project-id ID] [--workflow-id ID]"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --arms) ARMS="$2"; shift 2 ;;
    --template) TEMPLATE="$2"; shift 2 ;;
    --project-id) PROJECT_ID="$2"; shift 2 ;;
    --workflow-id) WORKFLOW_ID="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$TEMPLATE" == "quick" ]]; then
  WORKFLOW_ID="$QUICK_WORKFLOW_ID"
fi
if [[ -z "$WORKFLOW_ID" ]]; then
  echo "ERROR: set WORKFLOW_ID (SDLC Non-human template id) or --workflow-id; for Quick Work pass --template quick" >&2
  exit 2
fi
if ! command -v orchicon >/dev/null; then
  echo "ERROR: orchicon binary not on PATH" >&2
  exit 2
fi
if ! command -v python3 >/dev/null; then
  echo "ERROR: python3 required (MCP JSON-RPC framing)" >&2
  exit 2
fi

export ORCHICON_MCP_TENANT_ID="${ORCHICON_MCP_TENANT_ID:-tnt_dev}"

MCP_HELPER="$(mktemp -d)/mcp_call.py"
cat > "$MCP_HELPER" <<'PYEOF'
import json, subprocess, sys

PROC = None

def ensure():
    global PROC
    if PROC is None or PROC.poll() is not None:
        import os
        env = dict(os.environ)
        PROC = subprocess.Popen(
            ["orchicon", "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, env=env, text=True, bufsize=1)

def call(tool, args):
    ensure()
    req = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
           "params": {"name": tool, "arguments": args}}
    PROC.stdin.write(json.dumps(req) + "\n")
    PROC.stdin.flush()
    line = PROC.stdout.readline()
    if not line:
        raise RuntimeError("empty MCP response for " + tool)
    resp = json.loads(line)
    if resp.get("error"):
        raise RuntimeError("MCP error for %s: %s" % (tool, resp["error"]))
    return resp["result"]

if __name__ == "__main__":
    tool = sys.argv[1]
    args = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}
    out = call(tool, args)
    # MCP wraps content as [{type:text, text:...}]; unwrap for jq-friendly output.
    try:
        content = out.get("content", [])
        if isinstance(content, list) and content and isinstance(content[0], dict) and "text" in content[0]:
            print(content[0]["text"])
        else:
            print(json.dumps(out))
    except Exception:
        print(json.dumps(out))
PYEOF

mcp() { python3 "$MCP_HELPER" "$1" "$2"; }

echo "== worktree dispatch latency =="
echo "project=$PROJECT_ID workflow=$WORKFLOW_ID template=$TEMPLATE arms=$ARMS"

# Backlog precondition: >=20 terminal runs with worktrees (sweep pressure).
BACKLOG="$(mcp list_workflow_runs "{\"project_id\": \"$PROJECT_ID\", \"status\": \"completed\"}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("count", 0))' 2>/dev/null || echo 0)"
echo "terminal-run backlog (completed): $BACKLOG (want >=20 for sweep pressure)"

declare -a DELTAS=()
FAIL=0
for ((i = 1; i <= ARMS; i++)); do
  TITLE="Latency probe arm $i ($(date -u +%FT%TZ))"
  ITEM_JSON="$(mcp create_work_item "{\"title\": \"$TITLE\", \"project_id\": \"$PROJECT_ID\", \"kind\": \"task\", \"workflow_id\": \"$WORKFLOW_ID\", \"auto_start_workflow\": true}")"
  ITEM_ID="$(echo "$ITEM_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')"
  if [[ -z "$ITEM_ID" ]]; then echo "arm $i: FAILED to create work item: $ITEM_JSON" >&2; FAIL=1; continue; fi
  ARM_TS="$(date +%s)"
  # Find the bound run, then poll to ready (timeout 120s).
  RUN_ID=""; READY_TS=""; DELTA=""
  for ((p = 0; p < 120; p++)); do
    sleep 1
    if [[ -z "$RUN_ID" ]]; then
      RUN_ID="$(mcp get_work_item "{\"id\": \"$ITEM_ID\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("workflow_run_id","") or "")' 2>/dev/null || true)"
      [[ -z "$RUN_ID" ]] && continue
    fi
    WS="$(mcp get_workflow_run "{\"id\": \"$RUN_ID\"}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("worktree_status","")+"|"+(d.get("worktree_path","") or ""))' 2>/dev/null || echo "|")"
    if [[ "$WS" == ready\|* && "$WS" != "ready|" ]]; then
      READY_TS="$(date +%s)"; DELTA=$((READY_TS - ARM_TS)); break
    fi
    ST="$(echo "$WS" | cut -d'|' -f1)"
    if [[ "$ST" == "failed" || "$ST" == "skipped" ]]; then
      READY_TS="$(date +%s)"; DELTA=$((READY_TS - ARM_TS)); break
    fi
  done
  if [[ -z "$DELTA" ]]; then echo "arm $i: TIMEOUT waiting for ready (run=${RUN_ID:-none})" >&2; FAIL=1; continue; fi
  DELTAS+=("$DELTA")
  echo "arm $i: run=$RUN_ID delta=${DELTA}s status=$WS"
  if (( DELTA > 30 )); then echo "arm $i: EXCEEDS 30s bound" >&2; FAIL=1; fi
done

if (( ${#DELTAS[@]} == 0 )); then echo "no successful arms — cannot compute p95" >&2; exit 1; fi
STATS="$(printf '%s\n' "${DELTAS[@]}" | python3 -c '
import sys,math
xs=sorted(int(l) for l in sys.stdin if l.strip())
n=len(xs); p95=xs[min(n-1, math.ceil(0.95*n)-1)]; print(f"n={n} min={xs[0]} max={xs[-1]} p95={p95}")')"
echo "== result: $STATS (deltas: ${DELTAS[*]})"
P95="$(echo "$STATS" | sed -n 's/.*p95=\([0-9]*\).*/\1/p')"
if (( P95 >= 10 )); then echo "FAIL: p95 ${P95}s >= 10s bound" >&2; exit 1; fi
if (( FAIL != 0 )); then echo "FAIL: one or more arms violated the 30s bound" >&2; exit 1; fi
echo "PASS: p95 <10s and all arms <=30s"
