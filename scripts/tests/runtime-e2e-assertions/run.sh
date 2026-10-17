#!/usr/bin/env bash
# ============================================================================
# Offline harness for the RUNTIME-CONTAINER ACCEPTANCE HARNESS'S OWN HONESTY.
#
# WHY THIS EXISTS: the shape of this acceptance is ABSENCE-SHAPED. "no container
# was created", "the reused container was not replaced", "the gate rejected the
# create", "the local run invoked no engine" are all claims that are trivially
# true of a capture that FAILED TO RUN — `podman ps` that errored, an engine that
# is not installed, a daemon socket that is not there. A harness that turns those
# into PASS proves the opposite of what this item exists to prove. (Precedent: the
# no-engine harness once reported AC3 verified against a plane serving no UI.)
#
# WHAT IS PINNED HERE — the rule, not one call site:
#   1. An empty capture can NEVER produce a PASS. `gated_absent`/`gated_contains`
#      give SKIP (proven nothing) and record the reason in the step log.
#   2. A capture WITH bytes still behaves normally: PASS when the needle is
#      absent/present as expected, FAIL when it is not — and a FAIL names an
#      owner, so it can be filed as a defect (AC10).
#   3. The rule is STRUCTURAL: every step that reads an ENGINE capture (1, 3, 4,
#      5, 7, 8) contains NO raw `check_absent`/`check_contains`, so a future edit
#      cannot reintroduce the vacuous-PASS hole beside the gated ones.
#
# WHAT IS REAL HERE: the harness's own functions are SOURCED from
# scripts/runtime-e2e.sh and executed — `check`/`skip`/`record` run for real,
# writing real results.tsv/defects.tsv rows into a throwaway evidence dir under
# the repo's gitignored tmp/. No Postgres, no NATS, no port, no container engine,
# no database — which is why this gate is part of ci-go.
#
# SCOPE NOTE: the sourced harness defines its own `check`, so this runner's
# assertions are `expect` — a shared name would silently replace them.
#
#   scripts/tests/runtime-e2e-assertions/run.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
HARNESS="${REPO_ROOT}/scripts/runtime-e2e.sh"

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
spin "the harness is a library when sourced"
SOURCE_RC=0
# shellcheck source=../../runtime-e2e.sh
. "$HARNESS" >/dev/null 2>&1 || SOURCE_RC=$?
expect "sourcing scripts/runtime-e2e.sh never executes the session" 0 "$SOURCE_RC"
for fn in gated_absent gated_contains check_absent check_contains check skip record \
          engine_capture engine_bin connect_stream wait_run_terminal; do
  expect "the harness defines ${fn}() after sourcing" "yes" \
    "$([ "$(type -t "$fn")" = "function" ] && echo yes || echo no)"
done
for fn in step0_preflight step1_local_run step2_opt_in step3_container_run step4_pristine_reset \
          step5_pool_reuse step6_image_path step7_gate_check step8_all_native; do
  expect "the harness defines ${fn}() (one function per AC)" "yes" \
    "$([ "$(type -t "$fn")" = "function" ] && echo yes || echo no)"
done

# --- a real, throwaway evidence dir ---------------------------------------
mkdir -p "${REPO_ROOT}/tmp"
EVIDENCE="$(mktemp -d "${REPO_ROOT}/tmp/runtime-e2e-assertions.XXXXXX")" || exit 1
trap 'rm -rf "$EVIDENCE"' EXIT
# shellcheck disable=SC2034  # read by the sourced harness (step_file -> the SKIP reason log)
CURRENT_STEP=3
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
PASSES=0; FAILS=0; SKIPS=0

EMPTY="${EVIDENCE}/capture-empty.txt"; : >"$EMPTY"
WITH="${EVIDENCE}/capture-with-marker.txt"
printf 'orchicon-runtime-abc123   running\n' >"$WITH"
WITHOUT="${EVIDENCE}/capture-without-marker.txt"
printf 'something-else-abc999   running\n' >"$WITHOUT"
SKIPLOG="${EVIDENCE}/$(step_file)"

# --- rule 1: an EMPTY capture is SKIP, never PASS -------------------------
spin "rule 1: an empty capture proves nothing"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "no container carries the marker" "$EMPTY" "orchicon-runtime-abc123" "the engine capture is EMPTY" >/dev/null
expect "gated_absent on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_absent on an EMPTY capture counts no FAIL" 0 "$FAILS"
expect "gated_absent on an EMPTY capture counts one SKIP" 1 "$SKIPS"
expect "the SKIP records its reason in the step log" "yes" \
  "$(grep -qF "the engine capture is EMPTY" "$SKIPLOG" && echo yes || echo no)"
expect "the results.tsv row is SKIP, not PASS" "SKIP" \
  "$(awk -F'\t' '$2=="SKIP"{print $2}' "${EVIDENCE}/results.tsv" | tail -1)"

: >"${EVIDENCE}/results.tsv"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the engine lists the container" "$EMPTY" "orchicon-runtime-abc123" "the engine capture is EMPTY" >/dev/null
expect "gated_contains on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_contains on an EMPTY capture counts no FAIL" 0 "$FAILS"
expect "gated_contains on an EMPTY capture is SKIP" 1 "$SKIPS"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "no container carries the marker" "${EVIDENCE}/never-written.txt" "orchicon-runtime-abc123" "no capture" >/dev/null
expect "a MISSING capture is SKIP too (unwritten == empty)" 1 "$SKIPS"
expect "a MISSING capture counts no PASS" 0 "$PASSES"

# --- rule 2: a capture WITH bytes keeps real verdicts ---------------------
spin "rule 2: a real capture still yields real verdicts"
PASSES=0; FAILS=0; SKIPS=0
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
gated_absent "marker absent from a real capture" "$WITHOUT" "orchicon-runtime-abc123" "empty" >/dev/null
expect "gated_absent PASSes when the needle really is absent" 1 "$PASSES"
expect "gated_absent reports no SKIP for a real capture" 0 "$SKIPS"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "marker present in a real capture" "$WITH" "orchicon-runtime-abc123" "empty" >/dev/null
expect "gated_absent FAILs when the needle really is present (a defect, not a pass)" 1 "$FAILS"
expect "the FAIL is recorded in defects.tsv (it names an owner)" "yes" \
  "$([ -s "${EVIDENCE}/defects.tsv" ] && echo yes || echo no)"

PASSES=0; FAILS=0; SKIPS=0
gated_contains "marker present in a real capture" "$WITH" "orchicon-runtime-abc123" "empty" "internal/runtime/pool.go:98-155" >/dev/null
expect "gated_contains PASSes when the needle really is present" 1 "$PASSES"

PASSES=0; FAILS=0; SKIPS=0
gated_contains "marker absent from a real capture" "$WITHOUT" "orchicon-runtime-abc123" "empty" "internal/runtime/pool.go:98-155" >/dev/null
expect "gated_contains FAILs when the needle really is missing" 1 "$FAILS"
expect "the FAIL names the marker that was actually sought" "yes" \
  "$(grep -qF "orchicon-runtime-abc123" "${EVIDENCE}/defects.tsv" && echo yes || echo no)"
expect "the FAIL names the OWNING file:line (AC10)" "yes" \
  "$(grep -qF "internal/runtime/pool.go:98-155" "${EVIDENCE}/defects.tsv" && echo yes || echo no)"

# --- rule 3: the rule is structural, not one patched call site ------------
# Every one of these steps reads a capture produced by the ENGINE or by an
# install/build stream, i.e. the captures that are empty when nothing ran.
spin "rule 3: no RAW content assertion is left in any engine-reading step"
for fn in step1_local_run step3_container_run step4_pristine_reset step5_pool_reuse step7_gate_check step8_all_native; do
  BODY="$(sed -n "/^${fn}() {/,/^}/p" "$HARNESS")"
  expect "${fn} was extracted from the harness (vacuity guard)" "yes" \
    "$([ -n "$BODY" ] && echo yes || echo no)"
  expect "${fn} calls NO raw check_absent (an ABSENCE is the vacuous case)" 0 \
    "$(printf '%s\n' "$BODY" | grep -cE '(^|[^_[:alnum:]])check_absent ' || true)"
  expect "${fn} calls NO raw check_contains (every content read is gated)" 0 \
    "$(printf '%s\n' "$BODY" | grep -cE '(^|[^_[:alnum:]])check_contains ' || true)"
  expect "${fn} gates at least one assertion" "yes" \
    "$(printf '%s\n' "$BODY" | grep -q 'gated_absent\|gated_contains' && echo yes || echo no)"
done

# The two claims that MUST be provable from independent ground truth, not from
# the control plane's own report: container residency (AC3) and the gate's
# rejection (AC7). If these needles disappear, the assertions became reports.
spin "rule 3b: the decisive ACs read INDEPENDENT evidence"
expect "AC3 reads the engine's own ps output (not the plane's report)" "yes" \
  "$(sed -n '/^step3_container_run() {/,/^}/p' "$HARNESS" | grep -q 'engine_capture .*ps -a' && echo yes || echo no)"
expect "AC3 compares the in-container hostname against the host's" "yes" \
  "$(sed -n '/^step3_container_run() {/,/^}/p' "$HARNESS" | grep -q 'hostname' && echo yes || echo no)"
expect "AC7 attempts a REAL create on the daemon socket" "yes" \
  "$(sed -n '/^step7_gate_check() {/,/^}/p' "$HARNESS" | grep -q 'unix-socket' && echo yes || echo no)"
expect "AC2 asserts availability on /enginez" "yes" \
  "$(grep -q 'enginez?refresh=1' "$HARNESS" && echo yes || echo no)"
expect "AC2 has an ADVERSARIAL control (availability only after verify)" "yes" \
  "$(grep -q 'ORCHICON_ENGINE_VERIFY_IMAGE' "$HARNESS" && echo yes || echo no)"
expect "AC8 records the runtime-mode contradiction instead of reinterpreting it" "yes" \
  "$(sed -n '/^step8_all_native() {/,/^}/p' "$HARNESS" | grep -q 'lifecycle.go:958-975' && echo yes || echo no)"

# --- rule 4: an assertion that CANNOT pass is a false defect ---------------
# A vacuous PASS hides a real failure; an impossible assertion INVENTS one, and a
# harness that red-lights a correct system is worth as little as one that green-
# lights a broken one. Each of these was an assertion the product never made.
spin "rule 4: every assertion is satisfiable on a correct system"
expect "the E2E workflow is worker-free (an END step, not a task step with no ref)" "yes" \
  "$(grep -qE 'kind.{0,12}STEP_KIND_END' "$HARNESS" && ! grep -qE 'kind.{0,12}STEP_KIND_TASK' "$HARNESS" && echo yes || echo no)"
expect "the mode AC1 asserts is read from the PROJECT (the run has no such field)" "yes" \
  "$(grep -q '01-project-after-run.json' "$HARNESS" && ! grep -q '01-run-final.json" "EXECUTION_MODE_LOCAL' "$HARNESS" && echo yes || echo no)"
expect "the in-run marker is interpolated INTO the container exec command" "yes" \
  "$(sed -n '/^step3_container_run() {/,/^}/p' "$HARNESS" | grep -q 'runtime-e2e marker=' && echo yes || echo no)"
expect "the availability control uses an engine the probe cannot DRIVE (not a verify image)" "yes" \
  "$(sed -n '/^step2_opt_in() {/,/^}/p' "$HARNESS" | grep -q 'stub-bin' && echo yes || echo no)"
expect "the stream chunk count walks Connect ENVELOPES, not lines" "yes" \
  "$(grep -q 'connect_frames() {' "$HARNESS" && ! grep -q "grep -c ''" "$HARNESS" && echo yes || echo no)"

# connect_frames really counts envelopes: 3 data frames + a trailer is 3, so a
# streamed build cannot be denied for not streaming, and one blob is 1, so "more
# than one chunk" cannot be satisfied by a single message.
FRAMES="${EVIDENCE}/frames.bin"
printf '\x00\x00\x00\x00\x0b{"log":"a"}\x00\x00\x00\x00\x0b{"log":"b"}\x00\x00\x00\x00\x0b{"log":"c"}\x02\x00\x00\x00\x0b{"done":true}' >"$FRAMES"
expect "connect_frames counts 3 DATA frames (a real stream)" 3 "$(connect_frames "$FRAMES")"
printf '\x00\x00\x00\x00\x02{}\x02\x00\x00\x00\x02{}' >"${EVIDENCE}/one.bin"
expect "connect_frames counts a single-blob stream as 1 frame" 1 "$(connect_frames "${EVIDENCE}/one.bin")"
: >"${EVIDENCE}/empty.bin"
expect "connect_frames counts an EMPTY capture as 0 (never a vacuous pass)" 0 "$(connect_frames "${EVIDENCE}/empty.bin")"

# --- the gate must be DISCOVERABLE ----------------------------------------
spin "the gate is discoverable"
HELP="$(cd "$REPO_ROOT" && make help 2>/dev/null || true)"
expect "make help lists runtime-e2e-assertions-test" "yes" \
  "$(printf '%s\n' "$HELP" | grep -q 'runtime-e2e-assertions-test' && echo yes || echo no)"
expect "make ci-go runs this gate" "yes" \
  "$(grep -qE '^ci-go:.*runtime-e2e-assertions-test' "${REPO_ROOT}/Makefile" && echo yes || echo no)"
expect "the runbook exists and names the harness" "yes" \
  "$(grep -qF 'scripts/runtime-e2e.sh' "${REPO_ROOT}/docs/runtime-e2e-acceptance.md" 2>/dev/null && echo yes || echo no)"

echo
echo "  runtime-e2e-assertions harness: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
