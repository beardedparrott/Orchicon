#!/usr/bin/env bash
# ============================================================================
# Offline gate for the PACKAGED-DESKTOP ACCEPTANCE HARNESS'S OWN HONESTY.
#
# WHY THIS EXISTS: almost every claim in this item is ABSENCE-SHAPED. "The plane
# is down", "the banner is not shown", "the view did not freeze", "the offline
# panel named no URL" — each of those is trivially true of a capture that FAILED
# TO RUN (no display, no package, a curl that never connected). A harness that
# turns those into PASS proves the opposite of what this item exists to prove.
#
# WHAT IS PINNED HERE — the rule, not one call site:
#   1. An empty or missing capture can NEVER produce a PASS: gated_absent and
#      gated_contains give SKIP and record why in the step log.
#   2. A capture WITH bytes still behaves normally: PASS when the needle is
#      absent/present as expected, FAIL when it is not — and a FAIL names an
#      owner file:line, so the defect can be filed against the sibling task.
#   3. The rule is STRUCTURAL: no step function contains a raw
#      check_absent/check_contains, so a later edit cannot reintroduce the
#      vacuous-PASS hole beside the gated ones.
#   4. No step can PASS on the app's own report: every step body carries an
#      independent probe (/healthz, /versionz, dpkg -L, kill -0, ss, the plane's
#      own API counts, the loader cache).
#   5. The validity precondition REFUSES (exit 2) a run on a machine with a plane
#      already answering, and on a dev build — proven by running the harness
#      against fixture `curl`/`dpkg` shims in cases/.
#   6. Nothing is written outside the evidence directory (no mktemp, no /tmp).
#
# WHAT IS REAL HERE: the harness's own functions are SOURCED from
# scripts/desktop-e2e.sh and executed — check/skip/record/gated_* run for real,
# writing real results.tsv/defects.tsv rows into a throwaway dir under the repo's
# gitignored tmp/. No app, no plane, no display, no network — which is why this
# gate is part of `make ci`.
#
#   scripts/tests/desktop-e2e-assertions/run.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
HARNESS="${REPO_ROOT}/scripts/desktop-e2e.sh"
CASES="${HERE}/cases"

PASSED=0
FAILED=0

expect() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-60s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-60s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

spin() { echo; echo "  -- $* --"; }

# body_of <fn> — one step function's own text, matched with bash patterns rather
# than `sed | grep -q`: a `grep -q` that matches early closes the pipe, and that
# SIGPIPE made a sibling gate flake (observed once in seven runs).
body_of() { sed -n "/^$1() {/,/^}/p" "$HARNESS"; }
has()   { case "$1" in *"$2"*) echo yes ;; *) echo no ;; esac; }
hasnt() { case "$1" in *"$2"*) echo no ;; *) echo yes ;; esac; }
ins()   { printf '%s' "$1" | grep -Ec "$2" 2>/dev/null || true; }

# --- the harness must be SOURCEABLE ---------------------------------------
spin "the harness is a library when sourced"
SOURCE_RC=0
# shellcheck source=../../desktop-e2e.sh
. "$HARNESS" >/dev/null 2>&1 || SOURCE_RC=$?
expect "sourcing scripts/desktop-e2e.sh never executes the session" 0 "$SOURCE_RC"
for fn in record defect check skip check_absent check_contains gated_absent gated_contains \
          step_file step_log begin_step tee_step need_cmd need_file src_contains \
          plane_answers plane_get listen_pid is_dev_build installed_binary shot_assert \
          ocr_assert manual_sheet ctrl_port control_url abort_run; do
  expect "the harness defines ${fn}() after sourcing" "function" "$(type -t "$fn" 2>/dev/null || echo missing)"
done
for fn in step0_preflight step1_install step2_offline_state step3_start_from_app \
          step4_embedded_ui step5_streaming step6_unary_views step7_control_both_ways \
          step8_platform_variants print_plan parse_args main; do
  expect "the harness defines ${fn}() (one function per step)" "function" "$(type -t "$fn" 2>/dev/null || echo missing)"
done

# --- a real, throwaway evidence dir ---------------------------------------
mkdir -p "${REPO_ROOT}/tmp"
EVIDENCE="$(mktemp -d "${REPO_ROOT}/tmp/desktop-e2e-assertions.XXXXXX")" || exit 1
trap 'rm -rf "$EVIDENCE"' EXIT
CURRENT_STEP=2
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
PASSES=0; FAILS=0; SKIPS=0

EMPTY="${EVIDENCE}/capture-empty.txt"; : >"$EMPTY"
WITH="${EVIDENCE}/capture-with.txt"
printf 'orchicon desktop: the plane at http://127.0.0.1:8080 is unreachable — is it running?\n' >"$WITH"
WITHOUT="${EVIDENCE}/capture-without.txt"
printf 'nothing interesting here\n' >"$WITHOUT"

# --- rule 1: an EMPTY capture is SKIP, never PASS --------------------------
spin "rule 1: an empty capture proves nothing"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the offline panel names the URL" "$EMPTY" "http://127.0.0.1:8080" "the capture is EMPTY" >/dev/null
expect "gated_contains on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_contains on an EMPTY capture counts no FAIL" 0 "$FAILS"
expect "gated_contains on an EMPTY capture counts one SKIP" 1 "$SKIPS"
expect "the SKIP reason lands in the step log" "yes" "$(grep -qF 'the capture is EMPTY' "${EVIDENCE}/$(step_file)" && echo yes || echo no)"
expect "the results.tsv row is SKIP, not PASS" "SKIP" \
  "$(awk -F'\t' '$3=="SKIP"{print $3}' "${EVIDENCE}/results.tsv" | tail -1)"

: >"${EVIDENCE}/results.tsv"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "the banner is not shown" "$EMPTY" "version-mismatch" "the capture is EMPTY" >/dev/null
expect "gated_absent on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_absent on an EMPTY capture counts one SKIP" 1 "$SKIPS"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "the plane is down" "${EVIDENCE}/never-written.txt" "LISTEN" "no capture at all" >/dev/null
expect "a MISSING capture is SKIP too (unwritten == empty)" 1 "$SKIPS"
expect "a MISSING capture produces no PASS" 0 "$PASSES"

# --- rule 2: bytes still behave normally, and a FAIL names an owner --------
spin "rule 2: a capture with bytes still decides, and a FAIL names its owner"
: >"${EVIDENCE}/results.tsv"; : >"${EVIDENCE}/defects.tsv"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the proxy names the URL" "$WITH" "http://127.0.0.1:8080" "empty" >/dev/null
expect "gated_contains PASSes when the needle is present" 1 "$PASSES"
expect "and records no defect for a PASS" 0 "$(tail -n +2 "${EVIDENCE}/defects.tsv" | wc -l | tr -d ' ')"

PASSES=0; FAILS=0; SKIPS=0
gated_absent "the banner is not shown" "$WITHOUT" "version-mismatch" "empty" >/dev/null
expect "gated_absent PASSes when the needle is absent" 1 "$PASSES"

PASSES=0; FAILS=0; SKIPS=0
gated_contains "the proxy names the URL" "$WITHOUT" "http://127.0.0.1:8080" "empty" "internal/desktop/proxy.go:74-79" >/dev/null
expect "gated_contains FAILs when the needle is missing" 1 "$FAILS"
expect "the FAIL row carries the owner file:line" "internal/desktop/proxy.go:74-79" \
  "$(awk -F'\t' '$3=="FAIL"{print $5}' "${EVIDENCE}/results.tsv" | tail -1)"
expect "the FAIL writes a defect row naming the owning task" "yes" \
  "$(grep -qF 'internal/desktop/proxy.go:74-79' "${EVIDENCE}/defects.tsv" && echo yes || echo no)"
expect "the defect row has an id and a summary" "4" "$(awk -F'\t' 'NF{print NF}' "${EVIDENCE}/defects.tsv" | tail -1)"

# --- rule 3: the gates are used STRUCTURALLY -----------------------------
spin "rule 3: no step reads a capture without a gate"
for st in step1_install step2_offline_state step3_start_from_app step4_embedded_ui \
          step5_streaming step6_unary_views step7_control_both_ways step8_platform_variants; do
  b="$(body_of "$st")"
  expect "${st} contains no raw check_contains" no "$(has "$b" 'check_contains ')"
  expect "${st} contains no raw check_absent" no "$(has "$b" 'check_absent ')"
done
expect "step2 gates every captured body through gated_contains" "yes" \
  "$(has "$(body_of step2_offline_state)" 'gated_contains')"
expect "step5 gates the static streaming invariants through src_contains" "yes" \
  "$(has "$(body_of step5_streaming)" 'src_contains')"
expect "no step function calls a FAIL without an owner argument" "0" \
  "$(grep -Ec '^[[:space:]]*check .*" 1\)[[:space:]]*$' "$HARNESS" || true)"

# --- rule 4: no PASS on the app's own report ----------------------------
spin "rule 4: every step carries an INDEPENDENT probe"
expect "step1 proves the package from dpkg -L / sha256sum" "yes" \
  "$(has "$(body_of step1_install)" 'dpkg -L')"
expect "step1 records the installed binary fingerprint" "yes" \
  "$(has "$(body_of step1_install)" 'sha256sum')"
expect "step2 proves the URL from the proxy's own 502 body" "yes" \
  "$(has "$(body_of step2_offline_state)" '/healthz')"
expect "step2 reads the shell's own log line" "yes" \
  "$(has "$(body_of step2_offline_state)" 'plane version unavailable')"
expect "step3 probes /healthz independently of the app" "yes" \
  "$(has "$(body_of step3_start_from_app)" 'plane_answers /healthz')"
expect "step3 probes /versionz independently" "yes" "$(has "$(body_of step3_start_from_app)" '/versionz')"
expect "step3 resolves the listening PID independently" "yes" "$(has "$(body_of step3_start_from_app)" 'listen_pid')"
expect "step4 needs the injected desktop marker" "yes" "$(has "$(body_of step4_embedded_ui)" '__ORCHICON_DESKTOP__')"
expect "step4 FORCES a version mismatch instead of hoping for one" "yes" \
  "$(has "$(body_of step4_embedded_ui)" 'ORCHICON_DESKTOP_E2E_OTHER_PLANE')"
expect "step5 samples the plane's own event feed" "yes" \
  "$(has "$(body_of step5_streaming)" 'StreamExecutionEvents')"
expect "step5 compares three window frames (a frozen view must FAIL)" "yes" \
  "$(has "$(body_of step5_streaming)" '05-stream-t20.png')"
expect "step6 compares against the plane's own counts" "yes" \
  "$(has "$(body_of step6_unary_views)" 'ListProjects')"
expect "step7 verifies the plane is DOWN with curl, not the app's word" "yes" \
  "$(has "$(body_of step7_control_both_ways)" 'plane_answers /healthz 1')"
expect "step7 verifies the PID is gone" "yes" "$(has "$(body_of step7_control_both_ways)" 'kill -0')"
expect "step8 captures the Linux launcher message" "yes" \
  "$(has "$(body_of step8_platform_variants)" '08-launcher-nowebkit.txt')"

# --- rule 5: the validity precondition REFUSES with exit 2 ----------------
spin "rule 5: a run that cannot be a proof exits 2"
CHILD="${EVIDENCE}/child"; mkdir -p "$CHILD"
run_harness() { # run_harness <dir> [args...]
  local dir="$1"; shift
  ORCHICON_DESKTOP_E2E_NO_GATE=1 bash "$HARNESS" --evidence-dir "$dir" "$@" >/dev/null 2>&1
  echo $?
}

rc="$(run_harness "${CHILD}/plain" --control-port 59999)"
expect "a host with no package and no artifact REFUSES (exit 2)" 2 "$rc"
expect "the refusal is recorded as a SKIP row, never a PASS" "SKIP" \
  "$(awk -F'\t' '$2 ~ /valid proof/{print $3}' "${CHILD}/plain/results.tsv" | tail -1)"
expect "the refusal explains itself in summary.md" "yes" \
  "$(grep -qF 'RUN INVALID' "${CHILD}/plain/summary.md" && echo yes || echo no)"

mkdir -p "${CHILD}/shim-plane"
cp "${CASES}/fake-curl" "${CHILD}/shim-plane/curl"; chmod +x "${CHILD}/shim-plane/curl"
rc="$(PATH="${CHILD}/shim-plane:${PATH}" run_harness "${CHILD}/plane" --control-port 59999)"
expect "a machine with a plane ALREADY answering REFUSES (exit 2)" 2 "$rc"
expect "the refusal names the plane that answered" "yes" \
  "$(grep -qF 'already answering' "${CHILD}/plane/summary.md" && echo yes || echo no)"

mkdir -p "${CHILD}/devbuild/build/bin" && : >"${CHILD}/devbuild/build/bin/orchicon-desktop"
rc="$(run_harness "${CHILD}/dev" --control-port 59999 --deb "${CHILD}/devbuild/build/bin/orchicon-desktop")"
expect "a WAILS DEV BUILD is refused (exit 2)" 2 "$rc"
expect "the refusal names the dev build" "yes" \
  "$(grep -qiF 'dev build' "${CHILD}/dev/summary.md" && echo yes || echo no)"

# --- rule 6: nothing lands outside the evidence dir -----------------------
spin "rule 6: no scratch outside the evidence directory"
expect "the harness never uses mktemp" "0" "$(ins "$(cat "$HARNESS")" 'mktemp')"
expect "the harness never writes to /tmp" "0" "$(ins "$(cat "$HARNESS")" '/tmp/')"
expect "the harness has no sudo call (it is run with the rights it needs)" "0" \
  "$(ins "$(cat "$HARNESS")" 'sudo ')"
expect "the manual-sheet sheets land in the evidence dir" "yes" \
  "$(has "$(body_of manual_sheet)" '${EVIDENCE}/${CURRENT_STEP}-manual-')"

# --- rule 7: the contract is stated and honoured --------------------------
spin "rule 7: usage, exit contract, and a plan that executes nothing"
rc=0; bash "$HARNESS" --help >"${EVIDENCE}/help.txt" 2>&1 || rc=$?
expect "--help exits 0" 0 "$rc"
expect "--help documents the exit-2 (not a valid proof) contract" "yes" \
  "$(grep -qF 'not a valid proof' "${EVIDENCE}/help.txt" && echo yes || echo no)"
expect "--help documents that a dev build is inadmissible" "yes" \
  "$(grep -qiF 'dev build' "${EVIDENCE}/help.txt" && echo yes || echo no)"
rc=0; bash "$HARNESS" --dry-run --evidence-dir "${CHILD}/dry" >"${EVIDENCE}/dry.txt" 2>&1 || rc=$?
expect "--dry-run exits 0" 0 "$rc"
expect "--dry-run prints the plan and touches no step" "yes" \
  "$(grep -qF 'PLAN (nothing executed)' "${EVIDENCE}/dry.txt" && echo yes || echo no)"
expect "--dry-run writes no results row" "0" "$(tail -n +2 "${CHILD}/dry/results.tsv" 2>/dev/null | wc -l | tr -d ' ')"
expect "an unknown option exits 2" 2 "$(rc=0; bash "$HARNESS" --nonsense >/dev/null 2>&1 || rc=$?; echo $rc)"
expect "--platform windows produces the manual sheet, not a fake PASS" "yes" \
  "$(has "$(cat "$HARNESS")" 'manual_sheet "$PLATFORM"')"
expect "a SKIP is counted separately in summary.md" "yes" \
  "$(grep -qF 'A SKIP is UNPROVEN' "${CHILD}/plain/summary.md" && echo yes || echo no)"

# --- rule 8: the session's own machinery is honest about ITSELF ------------
spin "rule 8: the display, the exit code and the generated sheets"
HARNESS_SRC="$(cat "$HARNESS")"
expect "the session OWNS its X display instead of shooting a guessed one" "no" \
  "$(has "$HARNESS_SRC" 'DISPLAY:-:99')"
expect "the app and every screenshot share ONE display" "yes" \
  "$(has "$(body_of gui_launch)" 'gui_display')"
expect "gui_shot refuses to shoot a display it does not have" "yes" \
  "$(has "$(body_of gui_shot)" 'no session display')"
expect "gui_kill tears down the Xvfb the session started" "yes" \
  "$(has "$(body_of gui_kill)" 'XVFB_PID')"
expect "step 0 requires the X server it will also screenshot" "yes" \
  "$(has "$(body_of step0_preflight)" 'need_cmd Xvfb')"
expect "step 4 re-derives the shell origin after the mismatch relaunch" "yes" \
  "$(has "$(body_of step4_embedded_ui)" 'wait_shell_origin "$(step_log)"')"
# main must exit 1 for a FAILED step 0 in BOTH exits: --preflight-only and the
# whole session. One occurrence is the old "preflight always exits 0" hole.
expect "a FAILED step 0 is never reported as a clean preflight" "2" \
  "$(ins "$(body_of main)" 'finish 1')"
rc=0; bash "$HARNESS" --dry-run --evidence-dir "${CHILD}/notes" >/dev/null 2>&1 || rc=$?
expect "--dry-run exits 0 with an explicit evidence dir" 0 "$rc"
expect "the generated NOTES.md has no literal backslash-n" "0" \
  "$(ins "$(cat "${CHILD}/notes/NOTES.md" 2>/dev/null)" '\\n')"
expect "the generated NOTES.md names the session-owned display limit" "yes" \
  "$(grep -qF 'owns ONE Xvfb display' "${CHILD}/notes/NOTES.md" 2>/dev/null && echo yes || echo no)"
expect "the generated NOTES.md still states the OCR limit" "yes" \
  "$(grep -qF 'tesseract' "${CHILD}/notes/NOTES.md" 2>/dev/null && echo yes || echo no)"

# --- result ---------------------------------------------------------------
echo
if [ "$FAILED" -eq 0 ]; then
  printf '\033[32m%s assertions passed, 0 failed\033[0m\n' "$PASSED"
  exit 0
fi
printf '\033[31m%s assertions passed, %s FAILED\033[0m\n' "$PASSED" "$FAILED"
exit 1
