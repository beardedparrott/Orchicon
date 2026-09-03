#!/usr/bin/env bash
# No-synthesized-data CI gate (ADR-0010).
#
# Fails if any NON-TEST source (Go/TS/TSX) carries a synthesized-data
# fallback marker: a mock model/MCP discoverer, a "mockModels" catalog
# list, or a "MockProvider" double leaked into shipped code. This is the
# standing guarantee behind ADR-0010 (no synthesized data planes): a probe
# failure must degrade visibly to ZERO rows — never serve a model list that
# "seemingly exists".
#
# The ONE sanctioned simulation switch, ORCHICON_SIMULATE_ADAPTER (ADR-0010
# D2; opencode offline-dev opt-in), is deliberately NOT flagged — it is an
# explicit operator opt-in, never a fallback.
#
# Usage: scripts/check_no_synth_data.sh
#   exit 0 = no synthesized-data fallback in non-test source
#   exit 1 = a marker was found (path:line printed) or the sourcing
#            probe-or-nothing contract was removed
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Synthesized-data markers that must never appear in shipped adapter,
# discovery, or registry code.
readonly PATTERN='MockModelDiscoverer|MockMCPDiscoverer|mockModels|mockMCP|MockProvider'

# Walk only the source the audit owns; skip generated/vendored/build dirs.
# Test files (_test.go, *.test.ts, *.test.tsx) are excluded — the mock
# provider is intentionally a test-harness-only double.
mapfile -t hits < <(
  grep -RInE "$PATTERN" \
    --include='*.go' --include='*.ts' --include='*.tsx' \
    --exclude-dir=node_modules --exclude-dir=dist --exclude-dir=vendor \
    --exclude-dir=gen \
    . 2>/dev/null \
    | grep -vE '(_test\.go|\.test\.(ts|tsx))$' \
  || true
)

if [ "${#hits[@]}" -gt 0 ]; then
  echo "No-synthesized-data audit FAILED — synthesized data-plane marker(s) found in non-test source:" >&2
  printf '%s\n' "${hits[@]}" >&2
  echo "A probe failure must yield a visibly-degraded EMPTY list, never a model list that 'seemingly exists' (ADR-0010)." >&2
  exit 1
fi

# Structural probe-or-nothing guard: the sourcing service MUST keep its
# live-truth-only contract. If the sentinel is gone, the no-synthesized
# derivation was removed — fail loudly rather than let a catalog fallback
# sneak back in.
if ! grep -q 'LIVE TRUTH ONLY' internal/orchicon/sourcing.go; then
  echo "No-synthesized-data audit FAILED: internal/orchicon/sourcing.go lost its 'LIVE TRUTH ONLY' probe-or-nothing contract." >&2
  exit 1
fi

echo "No-synthesized-data audit OK: no synthesized data-plane fallback in non-test source."
