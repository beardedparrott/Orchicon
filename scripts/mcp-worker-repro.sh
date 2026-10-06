#!/usr/bin/env bash
# mcp-worker-repro.sh — run the click-level MCP repro for a WORKER page against a live plane.
#
# WHY A SCRIPT. The spec's own header documents the invocation, but the invocation has four env vars and
# a config path, and getting any of them wrong produces a SKIPPED test rather than an error — the quietest
# possible way to believe a repro ran when it did not. This fails loudly instead.
#
# THE WORKER MUST BE DISPOSABLE. Test (b) adds a catalog entry to the version and SAVES it, so point this
# at a scratch worker. Credentials are required rather than defaulted: a committed default password is a
# credential in the repository.
#
#   E2E_USERNAME=... E2E_PASSWORD=... scripts/mcp-worker-repro.sh <scratch-worker-id> [base-url]
#
# base-url defaults to the DEV instance (http://localhost:8080).
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "usage: E2E_USERNAME=... E2E_PASSWORD=... $0 <scratch-worker-id> [base-url]" >&2
  exit 2
fi
if [ -z "${E2E_USERNAME:-}" ] || [ -z "${E2E_PASSWORD:-}" ]; then
  echo "ERROR: set E2E_USERNAME and E2E_PASSWORD (no defaults — see this script's header)." >&2
  exit 2
fi

WORKER_ID="$1"
BASE="${2:-http://localhost:8080}"
cd "$(dirname "$0")/../frontend"

export E2E_WORKER_ID="$WORKER_ID"
export PLAYWRIGHT_BASE_URL="$BASE"

# --workers=1, both tests in order: (a) is client-only and (b) writes, and keeping them in one worker
# preserves the interaction between them (which is how the intermittent read-after-write window showed up
# as an order-dependent failure in the first place).
exec npx playwright test \
  --config playwright.mcp-repro.config.ts \
  tests/mcp-worker-catalog-bounce.spec.ts \
  --workers=1
