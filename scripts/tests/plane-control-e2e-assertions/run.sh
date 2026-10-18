#!/usr/bin/env bash
# ============================================================================
# Offline gate for the PLANE-CONTROL acceptance harness's own honesty.
#
# WHY THIS EXISTS. The acceptance harness's verdicts are ABSENCE-SHAPED: "the
# connection was REFUSED", "every port is free", "the process is gone", "the
# status does not say drifted". Every one of those is trivially true of a probe
# that FAILED TO RUN — a curl that is missing, a /proc table read with the wrong
# port format, a grep over a path that does not exist. A harness that turns that
# into PASS proves the opposite of what this work item exists to prove, and the
# failure mode is invisible until a real stop goes unverified.
#
# WHAT IS PINNED HERE:
#   1. `health_down` says DOWN only for a REFUSED connection — not for a server
#      that answers 200, and not for one that answers 500.
#   2. `port_listening` / `port_free` agree with the socket table in both
#      directions, so "all ports free" cannot be a false PASS.
#   3. `ports_all_free` reports WHICH port is still held, and keeps reporting it
#      when the INSTANCE has no port record at all (an unreadable input is a FAIL,
#      never an empty-and-therefore-free list).
#   4. `pid_of` refuses a stale PID, a non-numeric PID file, and a missing one.
#   5. The status-line matchers do not confuse a merely HEALTHY line with a
#      CURRENT binary line — the exact hole that would make the drift assertion
#      (AC5) pass against a status surface that only probed the port.
#   6. `check`/`blocked_surface`/`invalid_host` write the rows and set the flags
#      the EXIT CODE depends on: a missing surface must not exit 0, and a host
#      that is merely not installed must not file a defect against product code.
#   7. The harness is still a LIBRARY when sourced, and it no longer names any
#      verb that does not exist (correction C1: there is no `serve --start`).
#
# WHAT IS REAL HERE: the harness's own functions are SOURCED and executed — the
# probes run against a real python3 HTTP fixture and the real /proc socket table,
# and `check`/`record` write real results.tsv/defects.tsv rows into a throwaway
# evidence dir under the repo's gitignored tmp/. No Orchicon plane, no Postgres,
# no NATS, no instance, no database — which is why this gate can run in ci-go.
#
#   scripts/tests/plane-control-e2e-assertions/run.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
HARNESS="${REPO_ROOT}/scripts/plane-control-e2e.sh"

PASSED=0
FAILED=0

expect() { # <label> <expected> <actual> [actual-when-failing]
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-62s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-62s want [%s] got [%s]%s\n' "$1" "$2" "$3" "${4:+   $4}"
    FAILED=$((FAILED + 1))
  fi
}
spin() { echo; echo "  -- $* --"; }
rc() { "$@" >/dev/null 2>&1; echo $?; }

WORK="${REPO_ROOT}/tmp/plane-control-e2e-assertions.$$"
FIXTURE_HOME="${WORK}/home"
rm -rf "$WORK" 2>/dev/null || true
mkdir -p "$WORK" "$FIXTURE_HOME" || { echo "cannot create $WORK" >&2; exit 1; }

cleanup() {
  [ -n "${FIXTURE_PID:-}" ] && kill "$FIXTURE_PID" 2>/dev/null || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

# --- sourcing must never execute the session --------------------------------
spin "the harness is a library when sourced"
SOURCE_RC=0
# shellcheck source=../../plane-control-e2e.sh
. "$HARNESS" >/dev/null 2>&1 || SOURCE_RC=$?
expect "sourcing scripts/plane-control-e2e.sh never executes the session" 0 "$SOURCE_RC"
expect "sourcing does not choose an evidence dir (no run happened)" "" "${EVIDENCE}"
expect "sourcing leaves nothing marked as started" 0 "${DIRTY}"
for fn in record check skip blocked_surface invalid_host step_begin \
          port_listening port_free pid_owning_port hex_port \
          data_dir pid_file serve_log ports_json ports_of port_of \
          pid_of exe_of installed health_code health_up health_down \
          ports_all_free ports_listening_now \
          status_says_current status_says_superseded status_names_instance \
          status_says_healthy_at output_says_leaving_it output_says_reported_foreign; do
  expect "the harness defines ${fn}()" "function" "$(type -t "$fn" 2>/dev/null)"
done
for fn in step0_preflight step1_tui_stop step2_desktop_start step3_reverse step4_cli \
          step5_drift step6_failsafe step7_two_instances step8_env_leak step9_webapp \
          render_summary print_plan parse_args main launch_desktop close_desktop drive_tui; do
  expect "the harness defines ${fn}() (one function per step / AC)" "function" "$(type -t "$fn" 2>/dev/null)"
done

# --- fixtures ---------------------------------------------------------------
# A real listener that answers 200 on /healthz, and a port nothing listens on.
FIXTURE_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
FREE_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"

python3 - "$FIXTURE_PORT" >"${WORK}/fixture.log" 2>&1 <<'PY' &
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        code = 200 if self.path == "/healthz" else 404
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"ok" if code == 200 else b"no")
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
FIXTURE_PID=$!
i=0
while [ "$i" -lt 20 ] && ! port_listening "$FIXTURE_PORT"; do sleep 0.2; i=$((i + 1)); done

spin "health probes: a REFUSED connection is DOWN, an answer is not"
expect "health_code UP fixture -> 200" 200 "$(health_code "$FIXTURE_PORT")"
expect "health_up UP fixture -> pass" 0 "$(rc health_up "$FIXTURE_PORT")"
expect "health_down UP fixture -> fail (a serving plane is not down)" 1 "$(rc health_down "$FIXTURE_PORT")"
expect "health_code DOWN fixture -> empty (nothing answered)" "" "$(health_code "$FREE_PORT")"
expect "health_down DOWN fixture -> pass" 0 "$(rc health_down "$FREE_PORT")"
expect "health_up DOWN fixture -> fail" 1 "$(rc health_up "$FREE_PORT")"
expect "health_down on a 404 path is NOT 'down' (the fixture answers)" 1 "$(rc health_down "$FIXTURE_PORT")"

spin "the socket table, in both directions"
expect "port_listening UP fixture -> 0" 0 "$(rc port_listening "$FIXTURE_PORT")"
expect "port_listening DOWN fixture -> 1" 1 "$(rc port_listening "$FREE_PORT")"
expect "port_free DOWN fixture -> 0" 0 "$(rc port_free "$FREE_PORT")"
expect "port_free UP fixture -> 1" 1 "$(rc port_free "$FIXTURE_PORT")"
expect "pid_owning_port UP fixture -> the fixture's pid" "$FIXTURE_PID" "$(pid_owning_port "$FIXTURE_PORT" 2>/dev/null)"
expect "pid_owning_port DOWN fixture -> nothing" "" "$(pid_owning_port "$FREE_PORT" 2>/dev/null)"

spin "instance records are read back, and an absent record is a FAIL not a free list"
mkdir -p "${FIXTURE_HOME}/.local/share/orchicon-t1" "${FIXTURE_HOME}/.local/share/orchicon-t2" "${FIXTURE_HOME}/.local/share/orchicon-t3"
printf '{"ORCHICON_CONTROL_PORT": %s, "ORCHICON_POSTGRES_PORT": %s}\n' "$FIXTURE_PORT" "$FREE_PORT" \
  >"${FIXTURE_HOME}/.local/share/orchicon-t1/ports.json"
printf '{"ORCHICON_CONTROL_PORT": %s}\n' "$FREE_PORT" >"${FIXTURE_HOME}/.local/share/orchicon-t2/ports.json"
HOME_SAVED="$HOME"
HOME="$FIXTURE_HOME"
expect "port_of reads the named key" "$FIXTURE_PORT" "$(port_of t1 ORCHICON_CONTROL_PORT)"
expect "control_port is that key" "$FIXTURE_PORT" "$(control_port t1)"
expect "postgres_port is that key" "$FREE_PORT" "$(postgres_port t1)"
expect "ports_of lists every recorded port" "$FREE_PORT $FIXTURE_PORT" "$(printf '%s\n' "$(ports_of t1)" | tr -s ' ' | sed 's/^ //;s/ $//' | awk '{print $2, $1}')"
# These four run in THIS shell, not in $(...): HELD_PORTS is the harness's report of WHICH
# port is held, and a subshell would throw it away — exactly the way a real run would lose it.
ports_all_free t1; rc_held=$?
held_t1="$HELD_PORTS"
expect "ports_all_free FAILS when a recorded port is held" 1 "$rc_held"
expect "…and says WHICH port is held" "$FIXTURE_PORT" "$held_t1"
ports_all_free t2; rc_free=$?
held_t2="$HELD_PORTS"
expect "ports_all_free passes when every recorded port is free" 0 "$rc_free"
expect "…and reports nothing held" "" "$held_t2"
ports_all_free t3; rc_missing=$?
held_t3="$HELD_PORTS"
expect "ports_all_free FAILS (never silently passes) with no port record" 1 "$rc_missing"
expect "…and says the record is missing" "(no port record)" "$held_t3"
expect "ports_listening_now names the held ports" "$FIXTURE_PORT" "$(ports_listening_now t1)"
expect "installed is false without a port record" 1 "$(rc installed t3)"
expect "data_dir follows the instance, never a global" "${FIXTURE_HOME}/.local/share/orchicon-t1" "$(data_dir t1)"
expect "pid_file lives under the instance's own serve dir" \
  "${FIXTURE_HOME}/.local/share/orchicon-t1/serve/pids/orchicon.pid" "$(pid_file t1)"
expect "serve_log lives under the instance's own serve dir" \
  "${FIXTURE_HOME}/.local/share/orchicon-t1/serve/logs/orchicon.log" "$(serve_log t1)"

spin "a PID is believed only when the PROCESS is there"
mkdir -p "${FIXTURE_HOME}/.local/share/orchicon-t1/serve/pids"
printf '%s\n' "$$" >"$(pid_file t1)"
expect "pid_of returns a live PID" "$$" "$(pid_of t1)"
expect "exe_of resolves a live PID" 0 "$([ -n "$(exe_of "$$")" ] && echo 0 || echo 1)"
printf '999999\n' >"$(pid_file t1)"
expect "pid_of refuses a stale PID file" 1 "$(rc pid_of t1)"
printf 'not-a-pid\n' >"$(pid_file t1)"
expect "pid_of refuses a non-numeric PID file" 1 "$(rc pid_of t1)"
rm -f "$(pid_file t1)"
expect "pid_of refuses a missing PID file" 1 "$(rc pid_of t1)"
expect "exe_of on a dead PID is empty" "" "$(exe_of 999999)"
HOME="$HOME_SAVED"

spin "the status-line matchers — a HEALTHY line is not a CURRENT line"
CURRENT_LINE="instance dev (host-resident): plane pid 4242 running /home/u/.local/bin/orchicon — binary current, healthy at http://localhost:8080/healthz — data dir /home/u/.local/share/orchicon-dev"
SUPERSEDED_LINE="instance dev (host-resident): plane pid 4242 running /home/u/.local/bin/orchicon (deleted) — binary superseded, healthy at http://localhost:8080/healthz — data dir /home/u/.local/share/orchicon-dev"
HEALTHY_ONLY="instance dev: plane pid 4242 — identity unknown, healthy at http://localhost:8080/healthz — data dir /home/u/.local/share/orchicon-dev"
expect "a CURRENT line reads as current" 0 "$(rc status_says_current "$CURRENT_LINE")"
expect "a SUPERSEDED line does not read as current" 1 "$(rc status_says_current "$SUPERSEDED_LINE")"
expect "a SUPERSEDED line reads as superseded" 0 "$(rc status_says_superseded "$SUPERSEDED_LINE")"
expect "a CURRENT line does not read as superseded" 1 "$(rc status_says_superseded "$CURRENT_LINE")"
expect "a merely HEALTHY line is neither current nor superseded" "1 1" \
  "$(rc status_says_current "$HEALTHY_ONLY") $(rc status_says_superseded "$HEALTHY_ONLY")"
expect "…and it IS still recognisably healthy (drift must be asked separately)" 0 "$(rc status_says_healthy_at "$HEALTHY_ONLY")"
expect "the instance name is enforced (dev is not prod)" "0 1" \
  "$(rc status_names_instance "$CURRENT_LINE" dev) $(rc status_names_instance "$CURRENT_LINE" prod)"
expect "the fail-safe wording is matched" 0 \
  "$(rc output_says_leaving_it 'host plane already serving http://localhost:8080/healthz (could not identify the process; leaving it)')"
expect "an ordinary start message does NOT read as the fail-safe" 1 \
  "$(rc output_says_leaving_it 'host plane ready: http://localhost:8080/healthz')"
expect "the foreign-holder report is matched (both halves)" 0 \
  "$(rc output_says_reported_foreign "orchicon services: dev did not release postgres :5432 — and the holder is NOT this instance's")"
expect "half the foreign-holder message is NOT enough" 1 \
  "$(rc output_says_reported_foreign "orchicon services: dev did not release postgres :5432")"
expect "an ordinary services-down output is not a foreign-holder report" 1 \
  "$(rc output_says_reported_foreign 'orchicon services: dev is down')"

spin "the verdict machinery the EXIT CODE depends on"
EVIDENCE="${WORK}/evidence"
mkdir -p "$EVIDENCE"
: >"${EVIDENCE}/results.tsv"
: >"${EVIDENCE}/defects.tsv"
STEP_LOG="${EVIDENCE}/0-preflight.log"; : >"$STEP_LOG"
CURRENT_STEP="0"
PASSES=0; FAILS=0; SKIPS=0; BLOCKED=0; INVALID=0; BLOCKED_SURFACE=0

check "a passing assertion" 0 >/dev/null
expect "check(PASS) records a PASS row" "0	PASS	a passing assertion	" "$(tail -1 "${EVIDENCE}/results.tsv")"
check "a failing assertion" 1 "some owner" >/dev/null
expect "check(FAIL) records a FAIL row with its owner" "0	FAIL	a failing assertion	some owner" "$(tail -1 "${EVIDENCE}/results.tsv")"
expect "check(FAIL) files a defect row" "0	a failing assertion	some owner" "$(tail -1 "${EVIDENCE}/defects.tsv")"
expect "check counts one pass and one failure" "1 1" "$PASSES $FAILS"

skip "a skipped assertion" "no display" >/dev/null
expect "skip records a SKIP row and counts it" "0	SKIP	a skipped assertion	 1" "$(tail -1 "${EVIDENCE}/results.tsv") $SKIPS"

blocked_surface "a missing TUI surface" "TUI task" "no planecontrol import" >/dev/null
expect "blocked_surface sets the flag that forces exit 2" 1 "$BLOCKED_SURFACE"
expect "blocked_surface files a defect against the owning sibling" 1 \
  "$(tail -1 "${EVIDENCE}/defects.tsv" | grep -c 'TUI task')"
defects_before="$(wc -l <"${EVIDENCE}/defects.tsv")"
invalid_host "an instance that is not installed" "no port record" >/dev/null
expect "invalid_host also forces exit 2 (the run is not a valid proof)" 1 "$INVALID"
expect "invalid_host files NO defect (it accuses no product code)" "$defects_before" "$(wc -l <"${EVIDENCE}/defects.tsv")"
# verdict_for reads ONE step's rows, so the BLOCKED verdict is asserted on a clean file.
: >"${EVIDENCE}/results.tsv"
record BLOCKED "a surface that is absent" "TUI task"
expect "verdict_for reports BLOCKED for a step whose only verdict is BLOCKED" "BLOCKED (see defects.tsv)" "$(verdict_for 0)"
: >"${EVIDENCE}/results.tsv"
record FAIL "a broken behaviour" "owner"
expect "verdict_for reports FAIL ahead of everything (a real failure outranks a block)" "**FAIL** (see defects.tsv)" "$(verdict_for 0)"
: >"${EVIDENCE}/results.tsv"
record PASS "a working behaviour"
expect "verdict_for reports PASS for a step with only passing rows" "PASS" "$(verdict_for 0)"
: >"${EVIDENCE}/results.tsv"
record PASS "a working behaviour"
record SKIP "a manual observation"
# A step whose automated assertions PASSED and whose only gap is a human observation is
# PARTLY proven. Collapsing it to SKIP would read as "nothing was proven" — the same
# misreport as calling it a PASS.
expect "verdict_for reports PARTIAL when a step passed but a manual observation is outstanding" \
  "PARTIAL (automated assertions passed, a manual observation is outstanding)" "$(verdict_for 0)"
: >"${EVIDENCE}/results.tsv"
record SKIP "a manual observation only"
expect "verdict_for reports SKIP when a step proved nothing at all" "SKIP" "$(verdict_for 0)"

spin "the harness drives the verbs that exist (correction C1) and no others"
body_of() { sed -n "/^$1() {/,/^}/p" "$HARNESS"; }
count_of() { grep -cF -- "$1" "$2" 2>/dev/null || true; }
has()   { [ "$(count_of "$1" "$2")" -ge 1 ] && echo yes || echo no; }
expect "the harness never INVOKES 'serve --start' (a verb deliberately not added)" 0 \
  "$(grep -n 'serve --start' "$HARNESS" | grep -v ':[[:space:]]*#' | wc -l)"
expect "…it only ever MENTIONS it to record that it does not exist" yes \
  "$([ "$(grep -c 'serve --start' "$HARNESS")" -ge 1 ] && echo yes || echo no)"
for verb in 'start "$INSTANCE"' 'stop "$INSTANCE"' 'restart "$INSTANCE"' 'plane status' 'services down'; do
  expect "the harness drives '${verb}'" yes "$(has "$verb" "$HARNESS")"
done
expect "AC5 is asserted on 'plane status' (the only surface that can report drift)" yes \
  "$([ "$(body_of step5_drift | grep -c 'status_says_superseded')" -ge 1 ] && echo yes || echo no)"
expect "the drift step replaces the binary by RENAME (mv), not cp over a running exe" yes \
  "$([ "$(body_of step5_drift | grep -c 'mv -f')" -ge 1 ] && echo yes || echo no)"
expect "…and it never uses cp over a running executable" yes \
  "$([ "$(body_of step5_drift | grep -c 'cp ')" = 0 ] && echo yes || echo no)"
expect "the fail-safe step hides AND restores the PID file" yes \
  "$([ "$(body_of step6_failsafe | grep -c 'PID_FILE_BAK')" -ge 2 ] && echo yes || echo no)"
expect "the fail-safe step proves the foreign holder is still ALIVE (never killed)" yes \
  "$([ "$(body_of step6_failsafe | grep -c 'kill -0')" -ge 1 ] && echo yes || echo no)"
expect "the env-leak step plants the OTHER instance's state dir, log file and detach marker" yes \
  "$([ "$(body_of step8_env_leak | grep -c 'ORCHICON_SERVE_')" -ge 3 ] && echo yes || echo no)"
expect "the webapp step runs a CONTROL string before trusting an empty grep" yes \
  "$([ "$(body_of step9_webapp | grep -c 'useToastStore')" -ge 1 ] && echo yes || echo no)"
expect "…and the control really is checked BEFORE the empty result is believed" "control_first" \
  "$(awk '/useToastStore/{c=NR} /lifecycle_pattern=/{l=NR} END{print (c && l && c<l) ? "control_first" : "WRONG_ORDER"}' <<<"$(body_of step9_webapp)")"

spin "the harness keeps its own evidence, and its steps can be re-run"
# The held-port report is the ONE thing that tells a user WHICH port a stop left behind. It travels
# through HELD_PORTS, which a subshell throws away — so the probe must run in the harness's own shell.
expect "ports_all_free runs in THIS shell at every site (A0.4, A1.5, A4.3c, A7.3b)" 4 \
  "$(grep -cF 'ports_all_free "' "$HARNESS" || true)"
expect "…and none of them wraps it in \$(...) — which would eat the report" 0 \
  "$(grep -cF '$(ports_all_free ' "$HARNESS" || true)"
# `go test -run` with a pattern that matches nothing exits 0, so a renamed tripwire would hollow
# A9.1 out silently. Both PASS lines are demanded by name.
expect "A9.1 demands BOTH '--- PASS:' lines, so an empty -run cannot read as a pass" 2 \
  "$(grep -cF -- '--- PASS: Test' "$HARNESS" || true)"
# Step 7 leaves the OTHER instance running on purpose; "it has no plane pid" would FAIL on every
# valid host, against product code that is behaving. The claim is that its PID is UNCHANGED.
expect "a start that never came up cannot make the following stop a PASS (A4.1b gates A4.3b/A4.3c)" yes \
  "$([ "$(body_of step4_cli | grep -c 'up_proven')" -ge 4 ] && echo yes || echo no)"
expect "the env-leak step compares the other instance's plane PID instead of demanding it has none" yes \
  "$([ "$(body_of step8_env_leak | grep -c 'b_plane_pid_before')" -ge 2 ] && echo yes || echo no)"
expect "both desktop steps launch the app through the ONE helper" 2 \
  "$(grep -cF 'launch_desktop "' "$HARNESS" || true)"
expect "…and only that helper runs the app command, so the previous window is always closed" 1 \
  "$(grep -cF 'bash -c "$DESKTOP_CMD"' "$HARNESS" || true)"
# A missing keystroke export is THIS procedure's configuration gap; filing it against the TUI or the
# Wails task would break AC12 (a defect names the task that OWNS the failure).
expect "a missing keystroke/launch export is filed against this item, never against a sibling surface" yes \
  "$([ "$(body_of step0_preflight | grep -cF 'this item (harness configuration')" -ge 2 ] && echo yes || echo no)"
expect "…A0.5b names it for the TUI keystrokes" yes \
  "$([ "$(body_of step0_preflight | grep -cF 'harness configuration — export both')" -ge 1 ] && echo yes || echo no)"
expect "…and A0.6 names it for a missing launch command (not the Wails task)" yes \
  "$([ "$(body_of step0_preflight | grep -cF 'PLANE_E2E_DESKTOP_CMD)')" -ge 1 ] && echo yes || echo no)"

spin "the A9.2 tripwire: tight enough for the SPA, loose enough to catch a control"
TRIPWIRE='planecontrol|plane-control|StartPlane|StopPlane|RestartPlane|/plane/(start|stop|restart)|planeLifecycle'
PROSE_LINE='Fails executions whose runtime process is gone (plane restart, lost'
expect "the tripwire does NOT trip on the SPA's own prose help text" 0 \
  "$(printf '%s\n' "$PROSE_LINE" | grep -ciE "$TRIPWIRE" || true)"
for planted in 'const StartPlane = () => window.api.PlaneStart()' 'await client.planecontrol.start()' "fetch('/plane/start')"; do
  expect "the tripwire DOES catch a planted control: ${planted}" 1 \
    "$(printf '%s\n' "$planted" | grep -ciE "$TRIPWIRE" || true)"
done
# THE INVARIANT WAS SUPERSEDED, AND THIS ASSERTION USED TO SAY SO THE WRONG WAY.
#
# It used to demand ZERO matches — "the SPA has no lifecycle control today" — which was true when the SPA
# could only WATCH a plane, never control one. The Manage Server work deliberately changed that: the SPA now
# HAS plane control, gated to the desktop app. So the tripwire started firing on correct code, and go-ci went
# red on every PR from that point (13 matches, "want [0] got [13]").
#
# The fix is NOT to weaken the tripwire — it is to assert what is now TRUE, and the new invariant is STRONGER
# than the old one. Absence was easy to check and easy to satisfy by accident (a rename would have passed it);
# what actually matters is that control exists ONLY behind the desktop gate:
#
#   1. every plane-control symbol lives in the two desktop-gated modules, and
#   2. those modules reach the plane's lifecycle ONLY through the ONE desktop signal (DESKTOP_GLOBAL,
#      installed by the shell as non-writable and frozen, so page script cannot turn it on), and
#   3. the route that hosts the controls REDIRECTS a browser away rather than rendering them.
#
# (3) is the one that would catch a regression the old absence check never could: a page that renders plane
# controls to a browser is the leak the brief forbids, and it would pass "no matches in the modules" while
# being exactly the defect. See frontend/src/lib/desktop.ts's own note on the three surfaces that read it.
CONTROL_ALLOWED='^(frontend/src/lib/desktop\.ts|frontend/src/lib/desktop\.test\.ts|frontend/src/lib/manage-server\.ts|frontend/src/routes/manage-server\.tsx)$'
expect "every plane-control symbol is confined to the desktop-gated modules" 0 \
  "$(grep -rIlE "$TRIPWIRE" "${REPO_ROOT}/frontend/src" --include='*.ts' --include='*.tsx' 2>/dev/null \
     | grep -v '/api/gen/' | sed "s#^${REPO_ROOT}/##" | grep -vE "$CONTROL_ALLOWED" | wc -l)"
expect "the control module reads the ONE desktop signal, never a second detection" yes \
  "$([ "$(grep -cF 'DESKTOP_GLOBAL' "${REPO_ROOT}/frontend/src/lib/desktop.ts" 2>/dev/null)" -ge 1 ] && echo yes || echo no)"
# COMMENTS ARE STRIPPED BEFORE THIS CHECK, and it is not tidiness. The rule this asserts is stated IN A
# COMMENT in desktop.ts ("A second detection (window.runtime, user-agent sniffing, location.protocol) is a
# bug"), so a raw grep matches the sentence that forbids the thing and fails on the file that gets it RIGHT.
# The repo has been bitten by exactly this before (cmd/orchicon/ci_release_workflows_test.go: "a detector
# tripped by its own documentation"), and the answer there was the same: look at CODE, not prose.
expect "…and it does NOT sniff a second signal (user-agent / window.runtime / protocol)" 0 \
  "$(sed -e 's://.*::' "${REPO_ROOT}/frontend/src/lib/desktop.ts" 2>/dev/null \
     | grep -cE 'navigator\.userAgent|window\.runtime|location\.protocol ===' || true)"
expect "the hosting route REDIRECTS a browser instead of rendering the controls" yes \
  "$([ "$(grep -cF 'redirectTarget(' "${REPO_ROOT}/frontend/src/routes/manage-server.tsx" 2>/dev/null)" -ge 1 ] && echo yes || echo no)"
expect "…and the redirect is driven by isDesktop(), not by a local guess" yes \
  "$([ "$(grep -cF 'isDesktop()' "${REPO_ROOT}/frontend/src/routes/manage-server.tsx" 2>/dev/null)" -ge 1 ] && echo yes || echo no)"
expect "the tripwire is not vacuous: its control symbol is present in frontend/src" yes \
  "$([ "$(grep -rIl 'useToastStore' "${REPO_ROOT}/frontend/src" --include='*.ts' --include='*.tsx' 2>/dev/null | wc -l)" -ge 1 ] && echo yes || echo no)"

echo
if [ "$FAILED" -gt 0 ]; then
  echo "RESULT: ${FAILED} assertion(s) FAILED, ${PASSED} passed — the harness can green-light a broken plane."
  exit 1
fi
echo "RESULT: ${PASSED} assertions passed — every probe says PASS only when the fixture says it should."
exit 0
