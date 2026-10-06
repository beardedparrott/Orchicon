#!/usr/bin/env bash
# scripts/activity-line-e2e.sh — the activity-line end-to-end capstone runner.
#
# ONE fixture, TWO real clients: the real bin/orch in a real pty (asserted on the REPLAYED screen
# grid) and the real SPA in a real browser (asserted on the RENDERED DOM). Both legs drive the
# fixture's deterministic scripted turn; they agree through a file handshake at an ALIGNED SERVER
# CLOCK (qa-evidence/activity-line-e2e/tui-cross-client.json), so neither runtime needs the other's
# process to be alive at the same instant.
#
# WHY ONE PROVIDER LAYER IS SUBSTITUTED. A genuinely model-driven turn cannot run in this container:
# exec.LookPath("opencode") is empty and the serve-host fallback probe misses, so internal/opencode's
# ChatStream fails fast. The harness scripts the turn's events behind ChatStream and keeps every
# layer above it real. See the package doc in internal/testfixtures/activitye2e/plane.go and
# qa-report-activity-line-e2e.md §0.
#
# EACH LEG STARTS ITS OWN SERVER(s), so nothing here depends on a process a previous shell left
# running — and no leg can silently attach to a stale plane or the container's own sandbox plane:
#   * the Go test binds its OWN in-process plane (ORCH_ACTIVITY_E2E_ADDR, default 127.0.0.1:8080);
#   * the Playwright config (frontend/playwright.activity-e2e.config.ts) starts BOTH the standalone
#     plane on :18080 and the SPA dev server on :5174 as its webServer entries.
#
# Usage: scripts/activity-line-e2e.sh
# Fails on the first non-zero exit and names the AC whose evidence is missing.
set -uo pipefail

PROJ="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJ"

OUT="${ORCH_ACTIVITY_E2E_OUT:-$PROJ/qa-evidence/activity-line-e2e}"
mkdir -p "$OUT/logs"

# The container's default Go paths are unwritable; pin them under the project / private tmpfs.
export GOMODCACHE="${GOMODCACHE:-$PROJ/.dev/mod}"
export GOCACHE="${GOCACHE:-/tmp/orchicon/gocache}"
export GOTMPDIR="${GOTMPDIR:-/tmp/orchicon/gotmp}"
# Playwright's browser bundle is installed under the private tmpfs in this image.
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-/tmp/orchicon/pw}"
mkdir -p "$GOCACHE" "$GOTMPDIR"

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

step "1/4 build bin/orch"
make build 2>&1 | tee "$OUT/logs/build.log" || fail "make build"

step "2/4 TUI leg: the real binary in a real pty, asserted on the replayed frame"
ORCH_ACTIVITY_E2E=1 ORCH_PTY_SMOKE=1 \
  ORCH_ACTIVITY_E2E_ADDR="${ORCH_ACTIVITY_E2E_ADDR:-127.0.0.1:18081}" \
  ORCH_ACTIVITY_E2E_OUT="$OUT" \
  go test ./internal/tui -run 'TestActivityLineE2E$|TestActivityLineE2ETurnEnd|TestActivityLineE2EZeroToolCalls' -v -count=1 -timeout 900s \
  2>&1 | tee "$OUT/logs/tui-leg.log" || fail "TUI leg"

step "3/4 GUI leg: the real DOM, mid-reply (Playwright starts the plane + SPA itself)"
( cd frontend && npx playwright test \
    --config playwright.activity-e2e.config.ts --project=dark-desktop --reporter=list ) \
  2>&1 | tee "$OUT/logs/gui-leg.log" || fail "GUI leg"

step "4/4 evidence"
missing=""
for f in tui-cross-client.json gui-activity-line.txt gui-server-ledger.json; do
  [ -s "$OUT/$f" ] || missing="$missing $f"
done
if [ -n "$missing" ]; then
  echo "MISSING EVIDENCE for AC4/AC7/AC9:$missing" >&2
  exit 1
fi
echo "OK — evidence under $OUT"
