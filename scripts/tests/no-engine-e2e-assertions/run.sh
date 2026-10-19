#!/usr/bin/env bash
# ============================================================================
# Offline harness for the NO-ENGINE ACCEPTANCE HARNESS'S OWN HONESTY.
#
# WHY THIS EXISTS: the acceptance harness reported AC3 ("the change is live
# after the documented rebuild") as verified on a plane that served no UI at
# all. The assertions that read bundle CONTENT were plain `check_absent`/
# `check_contains` over a captured file, so a 0-byte capture turned
# "the marker is NOT in the served bundle" into a PASS. A harness that can say
# PASS about an empty file is worse than no harness: it manufactures the very
# blindness this feature exists to remove.
#
# WHAT IS PINNED HERE — the rule, not one call site:
#   1. An empty capture can NEVER produce a PASS. `gated_absent`/
#      `gated_contains` give SKIP (proven nothing) and record the reason, so the
#      gap stays visible in the step log instead of vanishing.
#   2. A capture WITH bytes still behaves normally: PASS when the needle is
#      absent/present as expected, FAIL when it is not — the gate must not
#      swallow real verdicts.
#   3. The rule is structural: step 4 (change + rebuild) contains no RAW
#      `check_absent`/`check_contains` call, so a future edit cannot reintroduce
#      the vacuous-PASS hole beside the gated ones.
#
# WHAT IS REAL HERE: the harness's own functions are SOURCED from
# scripts/no-engine-e2e.sh and executed — `check`/`skip`/`record` run for real,
# writing real `results.tsv`/`defects.tsv` rows into a throwaway evidence dir
# under the repo's gitignored tmp/. No Postgres, no NATS, no port, no container
# engine, no database — which is why this gate is part of ci-go.
#
# SCOPE NOTE: the sourced harness defines its own `check`, so this runner's
# assertions are `expect` — a shared name would silently replace them.
#
#   scripts/tests/no-engine-e2e-assertions/run.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
HARNESS="${REPO_ROOT}/scripts/no-engine-e2e.sh"

PASSED=0
FAILED=0

expect() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-58s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-58s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

spin() { echo; echo "  -- $* --"; }

# --- the harness must be SOURCEABLE ---------------------------------------
# A sourced harness that runs main() would download Postgres and bind ports; the
# runner needs the functions only, so `main` must be guarded by BASH_SOURCE.
spin "the harness is a library when sourced"
SOURCE_RC=0
# shellcheck source=../../no-engine-e2e.sh
. "$HARNESS" >/dev/null 2>&1 || SOURCE_RC=$?
expect "sourcing scripts/no-engine-e2e.sh never executes the session" 0 "$SOURCE_RC"
for fn in gated_absent gated_contains check_absent check_contains check skip record; do
  expect "the harness defines ${fn}() after sourcing" "yes" \
    "$([ "$(type -t "$fn")" = "function" ] && echo yes || echo no)"
done

# --- a real, throwaway evidence dir ---------------------------------------
mkdir -p "${REPO_ROOT}/tmp"
EVIDENCE="$(mktemp -d "${REPO_ROOT}/tmp/no-engine-e2e-assertions.XXXXXX")" || exit 1
trap 'rm -rf "$EVIDENCE"' EXIT
# shellcheck disable=SC2034  # read by the sourced harness (step_file -> the SKIP reason log)
CURRENT_STEP=4
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
PASSES=0; FAILS=0; SKIPS=0

EMPTY="${EVIDENCE}/capture-empty.txt"; : >"$EMPTY"
WITH="${EVIDENCE}/capture-with-marker.txt"
printf 'const banner = "MARKER-123";\n' >"$WITH"
WITHOUT="${EVIDENCE}/capture-without-marker.txt"
printf 'const banner = "previous build";\n' >"$WITHOUT"
SKIPLOG="${EVIDENCE}/$(step_file)"

# --- rule 1: an EMPTY capture is SKIP, never PASS -------------------------
spin "rule 1: an empty capture proves nothing"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "the marker is NOT in the empty capture" "$EMPTY" "MARKER-123" "the capture is EMPTY" >/dev/null
expect "gated_absent on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_absent on an EMPTY capture counts no FAIL" 0 "$FAILS"
expect "gated_absent on an EMPTY capture counts one SKIP" 1 "$SKIPS"
expect "the SKIP records its reason in the step log" "yes" \
  "$(grep -qF "the capture is EMPTY" "$SKIPLOG" && echo yes || echo no)"
expect "the results.tsv row is SKIP, not PASS" "SKIP" \
  "$(awk -F'\t' '$2=="SKIP"{print $2}' "${EVIDENCE}/results.tsv" | tail -1)"

: >"${EVIDENCE}/results.tsv"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the marker IS in the empty capture" "$EMPTY" "MARKER-123" "the capture is EMPTY" >/dev/null
expect "gated_contains on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_contains on an EMPTY capture counts no FAIL" 0 "$FAILS"
expect "gated_contains on an EMPTY capture is SKIP" 1 "$SKIPS"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "a capture that was never written" "${EVIDENCE}/never-written.txt" "MARKER-123" "no capture" >/dev/null
expect "a MISSING capture is SKIP too (unwritten == empty)" 1 "$SKIPS"
expect "a MISSING capture counts no PASS" 0 "$PASSES"

# --- rule 2: a capture WITH bytes keeps real verdicts ---------------------
spin "rule 2: a real capture still yields real verdicts"
PASSES=0; FAILS=0; SKIPS=0
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
gated_absent "marker absent from a real capture" "$WITHOUT" "MARKER-123" "empty" >/dev/null
expect "gated_absent PASSes when the needle really is absent" 1 "$PASSES"
expect "gated_absent reports no SKIP for a real capture" 0 "$SKIPS"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "marker present in a real capture" "$WITH" "MARKER-123" "empty" >/dev/null
expect "gated_absent FAILs when the needle really is present" 1 "$FAILS"
expect "the FAIL is recorded in defects.tsv (it names an owner)" "yes" \
  "$([ -s "${EVIDENCE}/defects.tsv" ] && echo yes || echo no)"

PASSES=0; FAILS=0; SKIPS=0
gated_contains "marker present in a real capture" "$WITH" "MARKER-123" "empty" >/dev/null
expect "gated_contains PASSes when the needle really is present" 1 "$PASSES"

PASSES=0; FAILS=0; SKIPS=0
gated_contains "marker absent from a real capture" "$WITHOUT" "MARKER-123" "empty" >/dev/null
expect "gated_contains FAILs when the needle really is missing" 1 "$FAILS"
expect "the FAIL names the marker that was actually sought" "yes" \
  "$(grep -qF "MARKER-123" "${EVIDENCE}/defects.tsv" && echo yes || echo no)"

# --- rule 3: the rule is structural, not one patched call site ------------
spin "rule 3: no RAW content assertion is left in step 4"
STEP4="$(sed -n '/^step4_rebuild_live() {/,/^}/p' "$HARNESS")"
expect "step 4 was extracted from the harness (vacuity guard)" "yes" \
  "$([ -n "$STEP4" ] && echo yes || echo no)"
expect "step 4 calls NO raw check_absent (an ABSENCE is the vacuous case)" 0 \
  "$(printf '%s\n' "$STEP4" | grep -cE '(^|[^_[:alnum:]])check_absent ' || true)"
# A raw check_CONTAINS is allowed and deliberate: an empty capture FAILS a
# presence assertion by construction, so it can never manufacture a PASS. Keep
# the list explicit so a new one is a reviewed decision, not a silent default.
expect "step 4's raw check_contains calls are the PRESENCE assertions only" 1 \
  "$(printf '%s\n' "$STEP4" | grep -cE '(^|[^_[:alnum:]])check_contains ' || true)"
expect "the one raw presence assertion is the BUILT-dist marker check" "yes" \
  "$(printf '%s\n' "$STEP4" | grep -qE '^  check_contains "the BUILT dist carries the new marker' && echo yes || echo no)"
expect "step 4 gates the SERVED bundle assertions" "yes" \
  "$([ "$(printf '%s\n' "$STEP4" | grep -c 'gated_absent\|gated_contains')" -ge 5 ] && echo yes || echo no)"
expect "step 4 gates the BUILT dist stale control" "yes" \
  "$(printf '%s\n' "$STEP4" | grep -q 'gated_absent "STALE-DIST CONTROL (BUILT)' && echo yes || echo no)"
expect "step 4 gates the BUILT dist escape-hatch assertion" "yes" \
  "$(printf '%s\n' "$STEP4" | grep -q 'gated_absent "the escape hatch clears the marker from the BUILT dist"' && echo yes || echo no)"

# --- the gate must be DISCOVERABLE ----------------------------------------
# A CI gate nobody can find from `make help` is a comment with a PASS line in
# it. It lives in ci-go and in the help table.
spin "the gate is discoverable"
HELP="$(cd "$REPO_ROOT" && make help 2>/dev/null || true)"
expect "make help lists no-engine-e2e-assertions-test" "yes" \
  "$(printf '%s\n' "$HELP" | grep -q 'no-engine-e2e-assertions-test' && echo yes || echo no)"
# THE QUESTION IS "IS THIS GATE PART OF ci-go", NOT "DOES ONE LINE NAME IT".
#
# This used to grep `^ci-go:.*no-engine-e2e-assertions-test` — a literal match on a single Makefile
# line. When the gate list moved into a CI_GO_GATES variable (so CI could run
# cross-compile as a parallel job), the gates were still in ci-go and this check
# could no longer SEE them, so it failed on correct code. Asking make itself is
# the property that matters and it survives any re-expression of the list.
# THE GATE MUST BE PART OF ci-go, and this reads the Makefile's OWN gate list
# rather than running make: `make -n ci-go` expands prerequisites, and `tools`
# fails without the pinned buf on PATH, so make stopped before printing the list
# and the check became environment-dependent — worse than the brittle literal it
# replaced. It accepts either expression (a literal `ci-go:` line, or the
# CI_GO_GATES variable the list now lives in), so re-expressing the list cannot
# silently drop the gate from the gate.
expect "make ci-go runs this gate" "yes" \
  "$(grep -qE "(^ci-go:|^CI_GO_GATES).*no-engine-e2e-assertions-test" "${REPO_ROOT}/Makefile" && echo yes || echo no)"

echo
echo "  no-engine-e2e-assertions harness: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
