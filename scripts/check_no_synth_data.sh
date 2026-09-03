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

# Scan only files git owns: tracked files + untracked-but-not-ignored files.
# This makes .gitignore authoritative — scratch dirs (.gotmp/, .qa-gotmp/,
# .orchicon-worktrees/, .dev/) hold stale worktree snapshots from past runs
# (including checkouts from the era when the mocks still existed) and a
# plain `grep -R .` would flag that debris as if it were shipped source.
# NUL-safe read (mapfile cannot parse -z output — bash C-string truncation).
# Test files (_test.go, _test.ts/_test.tsx, *.test.ts/*.test.tsx,
# *.spec.ts/*.spec.tsx) are excluded — the mock provider is intentionally
# a test-harness-only double.
declare -a files=()
while IFS= read -r -d '' f; do
  case "$f" in
    *_test.go|*_test.ts|*_test.tsx|*.test.ts|*.test.tsx|*.spec.ts|*.spec.tsx) continue ;;
  esac
  files+=("$f")
done < <(
  git ls-files --cached --others --exclude-standard -z -- '*.go' '*.ts' '*.tsx' 2>/dev/null
)

# Hard floor: if the file list is implausibly small the plumbing above broke
# (git missing, wrong cwd) — fail loudly rather than scan nothing and pass.
if [ "${#files[@]}" -lt 100 ]; then
  echo "No-synthesized-data audit FAILED: file enumeration returned only ${#files[@]} files — audit plumbing broken, not scanning." >&2
  exit 1
fi

readonly PATTERN='MockModelDiscoverer|MockMCPDiscoverer|mockModels|mockMCP|MockProvider'
declare -a hits=()
if [ "${#files[@]}" -gt 0 ]; then
  mapfile -t hits < <(
    printf '%s\0' "${files[@]}" \
    | xargs -0 grep -HnE "$PATTERN" -- 2>/dev/null \
    || true
  )
fi

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
