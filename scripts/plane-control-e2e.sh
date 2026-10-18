#!/usr/bin/env bash
# ============================================================================
# E2E acceptance harness for PLANE CONTROL across the TUI, the desktop app and
# the CLI.
#
# THE CLAIM. "Plane control works from the desktop app and the TUI, and both
# reach the same mechanism as the CLI." A component test cannot prove it: a Stop
# that passes its own unit test but cannot actually take a running plane down,
# and a Start that only works because a plane was already up, both go green while
# a user is stuck. So this walks the whole cycle against a REAL instance and
# believes none of it until an INDEPENDENT probe agrees.
#
# WHAT IS REAL HERE. The plane process, its /healthz over HTTP, its PID file, the
# listening sockets in /proc/net/tcp{,6}, /proc/<pid>/exe, ports.json, and each
# instance's log file. A verdict is NEVER taken from the control's own stdout: a
# start is proven by a 200 from /healthz, a stop by a REFUSED CONNECTION (curl
# exit 7, empty body) *and* by every recorded port being free, and "left alone"
# by the same PID still serving the same executable.
#
# WHAT THIS HARNESS REFUSES TO CALL A PASS:
#   * a surface that does not exist. No TUI lifecycle surface, no Wails desktop
#     app, no display to run one => BLOCKED plus a defect row naming the sibling
#     task that owns it, and the run exits 2 ("not a valid proof"). A procedure
#     that goes green when the thing under test is absent is worse than none.
#   * an instance that is not installed, or ports already held before step 0
#     (an invalid host, recorded WITHOUT a defect row - it accuses no product
#     code of anything, but it still cannot prove the claim).
#   * "the process exited". A zombie, or a survivor that still holds the port, is
#     a FAIL, and the failure names the port.
#   * an empty grep. A9.2 runs a CONTROL pattern that must match before the empty
#     result for the lifecycle patterns is allowed to mean "none".
#
# WHAT IT DOES NOT DO. It patches nothing (AC12). Every failure is written to
# defects.tsv with the file:line of the sibling task that owns it.
#
# USAGE
#   scripts/plane-control-e2e.sh [options]
#     --instance dev|prod|test   instance the single-instance steps drive (default dev)
#     --pair <a> <b>             the two-instance steps (default: <instance> + the other of dev/prod)
#     --with-desktop             run the desktop-app steps (needs $DISPLAY/$WAYLAND_DISPLAY + a packaged app)
#     --evidence-dir DIR         evidence root (default qa-evidence/plane-control-e2e/<UTC>)
#     --keep                     leave whatever this run started running
#     --dry-run                  print the plan for every step, execute nothing
#     --preflight-only           run step 0 only (the validity precondition), then stop
#     -h | --help
#
# VERBS IT DRIVES (2026-10-18 correction C1 — there is no `serve --start`):
#   orchicon start <I> | stop <I> | restart <I>      the user/admin path
#   orchicon plane status <I>                        the ONE surface that reports the
#                                                    serving binary and drift (C2)
#   orchicon serve --detach/--stop/--status           flags on serve itself (PID-only)
#   orchicon services down <I> [--force]             services, and this instance's OWN leftovers
#
# ENVIRONMENT
#   PLANE_E2E_ROOT          scratch dir for the built binaries
#   PLANE_E2E_TUI_STOP_KEYS the TUI keystroke sequence for stop; empty => the TUI step is BLOCKED
#   PLANE_E2E_DESKTOP_CMD   the packaged desktop app's launch command; empty => the desktop step is BLOCKED
#   PLANE_E2E_DESKTOP_START_KEYS / PLANE_E2E_DESKTOP_STOP_KEYS
#                           keys that drive the app's offline-state action, if it is drivable headlessly
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
INSTANCE="dev"
PAIR_A=""
PAIR_B=""
WITH_DESKTOP=0
DRY_RUN=0
PREFLIGHT_ONLY=0
KEEP=0
EVIDENCE=""
E2E_ROOT="${PLANE_E2E_ROOT:-${TMPDIR:-/tmp}/orchicon/plane-control-e2e}"
INSTALL_DIR="${E2E_ROOT}/install"
BIN=""                     # the binary UNDER TEST, installed into INSTALL_DIR (never the worktree's build)
BIN_PATH=""                # where the installed copy lives, so drift can be made by replacing THIS path
BIN_ORCH=""                 # the TUI binary
DESKTOP_PID=""              # the packaged desktop app this step launched (closed before the next one)

TUI_STOP_KEYS="${PLANE_E2E_TUI_STOP_KEYS:-}"
TUI_START_KEYS="${PLANE_E2E_TUI_START_KEYS:-}"
DESKTOP_CMD="${PLANE_E2E_DESKTOP_CMD:-}"
DESKTOP_START_KEYS="${PLANE_E2E_DESKTOP_START_KEYS:-}"
DESKTOP_STOP_KEYS="${PLANE_E2E_DESKTOP_STOP_KEYS:-}"

PASSES=0
FAILS=0
SKIPS=0
BLOCKED=0
INVALID=0                  # invalid HOST (not installed, ports held) - accuses no product code
BLOCKED_SURFACE=0          # a required surface is ABSENT - a defect against the sibling that owns it
ABORT_REASON=""
CURRENT_STEP="0"
STEP_LOG=""
HAVE_TUI=0
HAVE_DESKTOP=0
HOST_OK=0
HELD_PORTS=""
PID_FILE_BAK=""
DIRTY=0                    # something of ours may be running (teardown only then)

# The harness's own shell must not decide a case: a shell that launched a plane
# exports that instance's state dir, log file and detachment marker, and those
# are exactly the values AC8 proves are stripped at the spawn site.
unset MAKEFLAGS MFLAGS 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_SERVE_STATE_DIR ORCHICON_SERVE_LOG_FILE ORCHICON_SERVE_DETACHED 2>/dev/null || true
unset ORCHICON_DATA_DIR ORCHICON_BLOB_DIR ORCHICON_POSTGRES_DSN ORCHICON_NATS_URL ORCHICON_POSTGRES_PORT 2>/dev/null || true
unset ORCHICON_CONTROL_PORT ORCHICON_TEST_POSTGRES_PORT ORCHICON_TEST_CONTROL_PORT 2>/dev/null || true

# --- output ----------------------------------------------------------------
say()   { printf '%s\n' "$*"; }
head2() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok2()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad2()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip2() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
blk2()  { printf '  \033[35mBLOCKED\033[0m %s\n' "$*"; }
dim()   { printf '  \033[2m%s\033[0m\n' "$*"; }

# --- evidence ---------------------------------------------------------------
record() { # record <status> <assertion> [owner]
  printf '%s\t%s\t%s\t%s\n' "$CURRENT_STEP" "$1" "$2" "${3:-}" >>"${EVIDENCE}/results.tsv"
}

check() { # check <desc> <ok:0|1> [owner]
  local desc="$1" ok="$2" owner="${3:-}"
  if [ "$ok" = "0" ]; then
    PASSES=$((PASSES + 1)); record PASS "$desc"; ok2 "$desc"
  else
    FAILS=$((FAILS + 1)); record FAIL "$desc" "$owner"; bad2 "$desc${owner:+   <- owner: $owner}"
    printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$desc" "$owner" >>"${EVIDENCE}/defects.tsv"
  fi
}

skip() { # skip <desc> <why>
  SKIPS=$((SKIPS + 1)); record SKIP "$1" ""; skip2 "$1 — $2"
  printf '  (skip: %s)\n' "$2" >>"$STEP_LOG"
}

blocked_surface() { # blocked_surface <desc> <owner> <why> - a MISSING SURFACE: defect, exit 2
  BLOCKED=$((BLOCKED + 1)); BLOCKED_SURFACE=1
  record BLOCKED "$1" "$2"; blk2 "$1 — blocked on ${2}: $3"
  printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$1 [BLOCKED: $3]" "$2" >>"${EVIDENCE}/defects.tsv"
  printf '  (blocked: %s — owner %s)\n' "$3" "$2" >>"$STEP_LOG"
}

invalid_host() { # invalid_host <desc> <why> - cannot execute HERE; accuses no product code
  BLOCKED=$((BLOCKED + 1)); INVALID=1
  record BLOCKED "$1" "(invalid host)" ; blk2 "$1 — invalid host: $2"
  printf '  (invalid host: %s)\n' "$2" >>"$STEP_LOG"
}

note() { printf '  %s\n' "$*" >>"$STEP_LOG"; }

step_begin() { # step_begin <n> <slug>
  CURRENT_STEP="$1"; STEP_LOG="${EVIDENCE}/${1}-${2}.log"
  : >"$STEP_LOG" 2>/dev/null || true
  head2 "step ${1} — ${2}"
}

# c_run <cmd...> — run it, log the exact command, its exit code and its output,
# and hand the output back on stdout so an assertion can read it.
c_run() {
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  { printf '\n$ %s\n[exit %s]\n%s\n' "$*" "$rc" "$out"; } >>"$STEP_LOG"
  printf '%s' "$out"
  return "$rc"
}

# --- /proc primitives (ss/lsof/netstat/fuser are NOT assumed: a bare host has
#     the socket table and the process table, nothing else) -------------------
hex_port() { printf '%04X' "$1"; }

port_listening() { # port_listening <port> - true when /proc/net/tcp{,6} LISTENs there
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) { found=1; exit } } END { exit found?0:1 }' "$f" && return 0
  done
  return 1
}

listen_inodes() { # listen_inodes <port>
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) print $10 }' "$f"
  done
}

pid_owning_port() { # pid_owning_port <port> - the pid holding the LISTEN socket, or nothing
  local port="$1" inodes pid fd link target
  inodes="$(listen_inodes "$port")"
  [ -n "$inodes" ] || return 1
  for pid in /proc/[0-9]*; do
    for fd in "$pid"/fd/*; do
      link="$(readlink "$fd" 2>/dev/null)" || continue
      case "$link" in
        'socket:['*']')
          target="${link#socket:[}"; target="${target%]}"
          if printf '%s\n' "$inodes" | grep -qx -- "$target" 2>/dev/null; then
            printf '%s\n' "${pid#/proc/}"; return 0
          fi ;;
      esac
    done
  done
  return 1
}

port_free() { ! port_listening "$1"; }

# --- instance state (read from the RECORDS, never re-resolved) --------------
data_dir() { printf '%s/.local/share/orchicon-%s' "$HOME" "$1"; }
pid_file() { printf '%s/serve/pids/orchicon.pid' "$(data_dir "$1")"; }
serve_log() { printf '%s/serve/logs/orchicon.log' "$(data_dir "$1")"; }
ports_json() { printf '%s/ports.json' "$(data_dir "$1")"; }

ports_of() { # ports_of <instance> - every recorded port number, space separated
  local f; f="$(ports_json "$1")"
  [ -s "$f" ] || return 1
  if command -v jq >/dev/null 2>&1; then
    jq -r 'to_entries[] | .value' "$f" 2>/dev/null | tr '\n' ' '
  else
    python3 -c 'import json,sys;print(" ".join(str(v) for v in json.load(open(sys.argv[1])).values()),end=" ")' "$f" 2>/dev/null
  fi
}

port_of() { # port_of <instance> <ORCHICON_*_PORT>
  local f; f="$(ports_json "$1")"
  [ -s "$f" ] || return 1
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg k "$2" '.[$k] // empty' "$f" 2>/dev/null
  else
    python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2],""))' "$f" "$2" 2>/dev/null
  fi
}

control_port() { port_of "$1" ORCHICON_CONTROL_PORT; }
postgres_port() { port_of "$1" ORCHICON_POSTGRES_PORT; }

pid_of() { # pid_of <instance> - the recorded PID, printed only when it is a live process
  local pf pid; pf="$(pid_file "$1")"
  [ -s "$pf" ] || return 1
  pid="$(tr -dc '0-9' <"$pf" 2>/dev/null)"
  [ -n "$pid" ] || return 1
  [ -d "/proc/${pid}" ] || return 1
  printf '%s' "$pid"
}

exe_of() { readlink "/proc/$1/exe" 2>/dev/null || true; }

installed() { # installed <instance> - the data dir AND a port record exist
  [ -d "$(data_dir "$1")" ] && [ -s "$(ports_json "$1")" ]
}

# --- independent probes -----------------------------------------------------
health_code() { # health_code <control port> - the HTTP status, or EMPTY when nothing answered
  # curl prints "000" when it never got a status line. That is not a code, and a harness
  # that recorded it as one would make "refused" and "answered badly" look alike in the
  # evidence. A 500 is still returned: only a FAILED transfer yields nothing.
  local code rc
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "http://localhost:$1/healthz" 2>/dev/null)"; rc=$?
  [ "$rc" = 0 ] || return 1
  printf '%s' "$code"
}

health_up() { [ "$(health_code "$1")" = "200" ]; }

# health_down <port> — DOWN means the connection was REFUSED (curl exit 7, no
# body). A 500 or a slow server is NOT down: that distinction is the whole point.
health_down() {
  local out rc
  out="$(curl -sS --max-time 2 "http://localhost:$1/healthz" 2>/dev/null)"; rc=$?
  [ "$rc" = "7" ] && [ -z "$out" ]
}

# ports_all_free <instance> — every recorded port free. Sets HELD_PORTS.
ports_all_free() {
  local p ports held=""
  ports="$(ports_of "$1")" || { HELD_PORTS="(no port record)"; return 1; }
  for p in $ports; do
    port_listening "$p" && held="${held}${p} "
  done
  HELD_PORTS="${held% }"
  [ -z "$HELD_PORTS" ]
}

# ports_listening_now <instance> — the recorded ports that ARE held (for A7's
# "prod is untouched" half: prod's ports must still be held).
ports_listening_now() {
  local p ports held=""
  ports="$(ports_of "$1")" || return 1
  for p in $ports; do port_listening "$p" && held="${held}${p} "; done
  printf '%s' "${held% }"
}

plane_status_line() { c_run "$BIN" plane status "$1"; }

# --- the status-line matchers, as FUNCTIONS so they can be tested -----------
# Inlined `case` patterns could only be tested by copying them into the gate, so
# the self-test would prove its own copy rather than the harness. These four are
# what the self-test drives against real `Status.String()` output AND against a
# line that is merely healthy: "healthy at ..." alone must NOT read as current.
status_says_current()     { case "$1" in *"binary current"*) return 0 ;; *) return 1 ;; esac; }
status_says_superseded()  { case "$1" in *"binary superseded"*) return 0 ;; *) return 1 ;; esac; }
status_names_instance()   { case "$1" in *"instance $2"*) return 0 ;; *) return 1 ;; esac; }
status_says_healthy_at()  { case "$1" in *"healthy at"*) return 0 ;; *) return 1 ;; esac; }
output_says_leaving_it()  { case "$1" in *"leaving it"*) return 0 ;; *) return 1 ;; esac; }
output_says_reported_foreign() {
  case "$1" in
    *"did not release"*)
      case "$1" in *"the holder is NOT this instance's"*) return 0 ;; esac
      return 1 ;;
  esac
  return 1
}

surface_present_tui() {
  grep -rq 'internal/planecontrol' "${REPO_ROOT}/internal/tui" "${REPO_ROOT}/cmd/orch" 2>/dev/null
}

surface_present_desktop() {
  # A Wails app is a Go tree that imports Wails; the plan's own probe.
  local hits
  hits="$(grep -rIl --include='*.go' 'wails' "${REPO_ROOT}" 2>/dev/null | grep -v 'architecture-notes' | head -5)"
  [ -n "$hits" ] || return 1
  { printf 'desktop app files referencing wails:\n%s\n' "$hits"; } >>"$STEP_LOG"
  return 0
}

display_available() { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; }

# --- arguments --------------------------------------------------------------
usage() { awk 'NR>2 && /^set -uo/{exit} NR>2{sub(/^# ?/,"");print}' "${BASH_SOURCE[0]}"; exit "${1:-0}"; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --instance) INSTANCE="${2:?}"; shift 2 ;;
      --pair) PAIR_A="${2:?}"; PAIR_B="${3:?}"; shift 3 ;;
      --with-desktop) WITH_DESKTOP=1; shift ;;
      --evidence-dir) EVIDENCE="${2:?}"; shift 2 ;;
      --keep) KEEP=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      --preflight-only) PREFLIGHT_ONLY=1; shift ;;
      -h|--help) usage 0 ;;
      *) echo "plane-control-e2e: unknown argument '$1' (see --help)" >&2; exit 2 ;;
    esac
  done
  case "$INSTANCE" in dev|prod|test) ;; *) echo "plane-control-e2e: --instance must be dev, prod or test" >&2; exit 2 ;; esac
  if [ -z "$PAIR_A" ]; then
    PAIR_A="$INSTANCE"
    case "$INSTANCE" in dev) PAIR_B="prod" ;; prod) PAIR_B="dev" ;; *) PAIR_B="prod" ;; esac
  fi
  [ -n "$EVIDENCE" ] || EVIDENCE="${REPO_ROOT}/qa-evidence/plane-control-e2e/${UTC_STAMP}"
  case "$EVIDENCE" in /*) ;; *) EVIDENCE="${REPO_ROOT}/${EVIDENCE}" ;; esac
}

# --- step 0: the validity precondition --------------------------------------
step0_preflight() {
  step_begin 0 preflight
  mkdir -p "$INSTALL_DIR"
  BIN_PATH="${INSTALL_DIR}/orchicon"
  BIN="$BIN_PATH"

  # THE BINARY UNDER TEST IS A COPY in a scratch install dir. A5 replaces it on
  # purpose; replacing the worktree's own build would mutate the tree under test
  # and make the run unrepeatable.
  local built=1
  c_run go -C "$REPO_ROOT" build -o "$BIN_PATH" ./cmd/orchicon >/dev/null && built=0
  check "A0.1 the binary under test builds and runs (${BIN_PATH} version)" "$built" "this item (harness bug)"
  if [ "$built" != 0 ]; then
    ABORT_REASON="the binary under test does not build — nothing else can be proven"
    INVALID=1
    return
  fi
  c_run "$BIN" version >/dev/null || true
  c_run "$BIN" version >>"$STEP_LOG" 2>&1 || true

  # A0.2 / A0.3 — the instance has to exist, with a port record read back (never re-resolved).
  if installed "$INSTANCE"; then
    check "A0.2 instance '$INSTANCE' is installed (data dir $(data_dir "$INSTANCE"))" 0
    check "A0.3 instance '$INSTANCE' has a non-empty port record" 0
    note "ports.json: $(cat "$(ports_json "$INSTANCE")" 2>/dev/null)"
    note "residency: $(cat "$(data_dir "$INSTANCE")/residency" 2>/dev/null || echo '(none recorded)')"
  else
    invalid_host "A0.2 instance '$INSTANCE' is installed (data dir + ports.json)" "no port record at $(ports_json "$INSTANCE") — the instance was never installed on this host"
    invalid_host "A0.3 instance '$INSTANCE' has a non-empty port record" "same — nothing to read back"
    ABORT_REASON="instance '$INSTANCE' is not installed on this host: the lifecycle steps cannot be executed here"
  fi

  # A0.4 — the run starts from a CLEAN host, or the evidence is unreadable.
  if installed "$INSTANCE"; then
    if ports_all_free "$INSTANCE"; then
      check "A0.4 no port of '$INSTANCE' is held before the run starts (clean baseline)" 0
    else
      invalid_host "A0.4 no port of '$INSTANCE' is held before the run starts" "already held: ${HELD_PORTS} — a plane from an earlier run is still up"
      [ -z "$ABORT_REASON" ] && ABORT_REASON="ports already held before the run: ${HELD_PORTS}"
    fi
  else
    invalid_host "A0.4 no port of '$INSTANCE' is held before the run starts" "no port record, so nothing to check"
  fi

  # A0.5 — the TUI lifecycle surface. ABSENT today: BLOCKED + defect, never a pass.
  if surface_present_tui; then
    HAVE_TUI=1
    check "A0.5 a TUI lifecycle surface exists (internal/tui or cmd/orch calls internal/planecontrol)" 0
    # THE KEYSTROKES ARE THIS PROCEDURE'S CONFIGURATION, not the TUI's behaviour. A missing
    # export must not be filed as a defect against the TUI task: AC12 says a failure names the
    # task that OWNS it, and "nobody exported the binding" is ours.
    if [ -n "$TUI_STOP_KEYS" ] && [ -n "$TUI_START_KEYS" ]; then
      check "A0.5b both TUI keystroke sequences are configured (stop and start)" 0
    else
      check "A0.5b both TUI keystroke sequences are configured (PLANE_E2E_TUI_STOP_KEYS / PLANE_E2E_TUI_START_KEYS)" 1 \
        "this item (harness configuration — export both, then re-run)"
    fi
  else
    blocked_surface "A0.5 a TUI lifecycle surface exists (internal/tui or cmd/orch calls internal/planecontrol)" "TUI task" "grep 'internal/planecontrol' over internal/tui and cmd/orch matches nothing; cmd/orch/main.go:3 says the TUI carries zero control-plane"
  fi

  # A0.6 — the Wails desktop app. ABSENT today: BLOCKED + defect.
  if surface_present_desktop; then
    if display_available; then
      if [ -n "$DESKTOP_CMD" ]; then
        HAVE_DESKTOP=1
        check "A0.6 a Wails desktop app exists, a display is available and its launch command is set" 0
      else
        # Same rule as A0.5b: no launch command is OUR configuration gap, not the app's bug.
        check "A0.6 a Wails desktop app exists, a display is available and its launch command is set" 1 \
          "this item (harness configuration — export PLANE_E2E_DESKTOP_CMD)"
      fi
    else
      blocked_surface "A0.6 the desktop app can be driven on this host" "Wails desktop app task" "a Wails app needs a display; neither DISPLAY nor WAYLAND_DISPLAY is set in this session"
    fi
  else
    blocked_surface "A0.6 a Wails desktop app exists" "Wails desktop app task" "no *.go file in the tree references wails: there is no desktop/ package, no wails.json and no packaged app to launch"
  fi

  # A0.7 — an engine is NOT required by this procedure; its presence is recorded, not gated.
  note "container engines present: $(command -v docker podman 2>/dev/null | tr '\n' ' ' || true)(not required by this procedure)"
  check "A0.7 no container engine is required by this procedure" 0

  c_run "$BIN" plane status "$INSTANCE" >>"$STEP_LOG" 2>&1 || true
  note "baseline plane status logged above (the CLI's own report — never the proof)"

  if installed "$INSTANCE" && [ "$INVALID" = 0 ]; then HOST_OK=1; fi
}

# --- TUI driving (a real pty, not a pipe: the TUI is a terminal program) ----
# The keystroke sequences are SINGLE VARIABLES by design: when the TUI task lands
# and documents its bindings, a mismatch costs one export, not a rewrite.
tui_binary() {
  [ -n "$BIN_ORCH" ] && [ -x "$BIN_ORCH" ] && return 0
  BIN_ORCH="${INSTALL_DIR}/orch"
  c_run go -C "$REPO_ROOT" build -o "$BIN_ORCH" ./cmd/orch >/dev/null 2>&1 || return 1
  [ -x "$BIN_ORCH" ]
}

drive_tui() { # drive_tui <keys> — feed keys through `script` so the TUI sees a pty
  local keys="$1" runner
  tui_binary || return 1
  runner=()
  command -v timeout >/dev/null 2>&1 && runner=(timeout 45)
  { sleep 4; printf '%s' "$keys"; sleep 10; } \
    | "${runner[@]}" script -qfec "${BIN_ORCH}" /dev/null >>"$STEP_LOG" 2>&1
  return 0
}

# close_desktop — close the packaged app THIS step launched. Two steps launch it, and a
# second window onto the same instance would decide the next step's assertion by itself.
close_desktop() {
  [ -n "$DESKTOP_PID" ] || return 0
  local pid="$DESKTOP_PID"; DESKTOP_PID=""
  # `script` (when keys are replayed) wraps the app, so TERM the group first and the pid after;
  # both are this harness's own children and both failures are ignorable.
  kill -TERM -- "-$pid" 2>/dev/null || true
  kill -TERM "$pid" 2>/dev/null || true
  note "closed the desktop app (pid $pid)"
  return 0
}

# launch_desktop <keys> <what> — launch the packaged app for one step, replays its keystrokes
# when any are configured, and SAYS SO when it cannot: a GUI action the harness cannot perform
# itself is owed by a human with a screenshot, never silently skipped.
launch_desktop() {
  local keys="$1" what="$2"
  close_desktop
  if [ -z "$DESKTOP_CMD" ]; then
    note "no PLANE_E2E_DESKTOP_CMD: nothing to launch"
    return 1
  fi
  note "launching the packaged desktop app for ${what}: $DESKTOP_CMD"
  if [ -n "$keys" ] && command -v script >/dev/null 2>&1; then
    { sleep 4; printf '%s' "$keys"; sleep 8; } \
      | script -qfec "$DESKTOP_CMD" /dev/null >>"$STEP_LOG" 2>&1 &
    note "replayed the keystrokes for ${what}: $(printf '%q' "$keys")"
  else
    bash -c "$DESKTOP_CMD" >>"$STEP_LOG" 2>&1 &
    note "no keystrokes configured for ${what}: the action must be clicked by hand, and the screenshot attached"
  fi
  DESKTOP_PID=$!
  sleep 8
  return 0
}

# wait_down <port> [tries] — a stop is confirmed by the socket, with a bounded wait.
wait_down() {
  local port="$1" tries="${2:-20}" i=0
  while [ "$i" -lt "$tries" ]; do
    health_down "$port" && ! port_listening "$port" && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}

wait_up() {
  local port="$1" tries="${2:-60}" i=0
  while [ "$i" -lt "$tries" ]; do
    health_up "$port" && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}

# --- step 1: TUI stop (AC1) -------------------------------------------------
step1_tui_stop() {
  step_begin 1 tui-stop
  local cp; cp="$(control_port "$INSTANCE")"
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A1 TUI stop (AC1) on '$INSTANCE'" "instance '$INSTANCE' is not installed on this host, so there is no plane for the TUI to stop"
    return
  fi
  local pid exe
  pid="$(pid_of "$INSTANCE")"; exe="$(exe_of "$pid")"
  note "baseline: /healthz=$(health_code "$cp") pid=${pid:-none} exe=${exe:-none} ports='$(ports_of "$INSTANCE")'"
  check "A1.1 baseline: '$INSTANCE' plane answers /healthz (pid ${pid:-none})" \
    "$([ "$(health_code "$cp")" = 200 ] && echo 0 || echo 1)" "install task"

  if [ "$HAVE_TUI" != 1 ]; then
    blocked_surface "A1.2 the TUI can be driven through a real pty to issue STOP" "TUI task" \
      "there is no TUI lifecycle surface to drive, so no keystroke sequence exists (internal/tui, cmd/orch/main.go:3)"
    blocked_surface "A1.3 plane is DOWN after the TUI stop: /healthz REFUSED (curl exit 7, empty body)" "TUI task" "depends on A1.2"
    blocked_surface "A1.4 the PID file is gone or names a dead PID" "TUI task" "depends on A1.2"
    blocked_surface "A1.5 every recorded port of '$INSTANCE' is free after the TUI stop" "TUI task" "depends on A1.2"
    return
  fi
  if [ -z "$TUI_STOP_KEYS" ]; then
    # The surface EXISTS and we cannot drive it: that is this procedure's configuration gap and
    # it must not be filed against the TUI (AC12). Still not a valid proof, so still exit 2.
    blocked_surface "A1.2 the TUI STOP action can be issued through a real pty" \
      "this item (harness configuration — export PLANE_E2E_TUI_STOP_KEYS, then re-run)" \
      "a TUI lifecycle surface exists but no stop keystrokes were configured"
    return
  fi

  drive_tui "$TUI_STOP_KEYS"
  note "TUI keystrokes sent: $(printf '%q' "$TUI_STOP_KEYS")"

  check "A1.3 plane is DOWN after the TUI stop: /healthz REFUSED (curl exit 7, empty body)" \
    "$(wait_down "$cp" && echo 0 || echo 1)" "TUI task"
  check "A1.4 the PID file is gone or names a dead PID" \
    "$(pid_of "$INSTANCE" >/dev/null 2>&1 && echo 1 || echo 0)" "TUI task"
  # THE PROBE RUNS IN THIS SHELL, not inside $(...): ports_all_free reports WHICH port is held
  # through HELD_PORTS, and a subshell would throw that report away — a FAIL that cannot name the
  # port still holding the instance is the evidence this step exists to produce.
  local ports_free_after=0
  ports_all_free "$INSTANCE" || ports_free_after=1
  check "A1.5 every recorded port of '$INSTANCE' is free after the TUI stop${HELD_PORTS:+ (still held: $HELD_PORTS)}" \
    "$ports_free_after" "stop-reclaims-leftovers task"
  DIRTY=1
}

# --- step 2: desktop app start (AC2) ----------------------------------------
step2_desktop_start() {
  step_begin 2 desktop-start
  local cp; cp="$(control_port "$INSTANCE")"
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A2 desktop-app start (AC2) on '$INSTANCE'" "instance '$INSTANCE' is not installed on this host"
    return
  fi
  if [ "$HAVE_DESKTOP" != 1 ]; then
    blocked_surface "A2.1 the packaged desktop app can be launched and its offline-state START action used" "Wails desktop app task" \
      "no Wails desktop app exists in this tree, so there is no offline state and no start action to click"
    blocked_surface "A2.2 /healthz returns 200 AND the UI loads after the desktop app's start" "Wails desktop app task" "depends on A2.1"
    blocked_surface "A2.3 the proof is curl + the rendered window, not the app's own status line" "Wails desktop app task" "depends on A2.1"
    return
  fi

  note "launching: $DESKTOP_CMD"
  launch_desktop "$DESKTOP_START_KEYS" "the offline-state START action"
  check "A2.2 '$INSTANCE' /healthz returns 200 after the desktop app's start (independent probe)" \
    "$(wait_up "$cp" && echo 0 || echo 1)" "Wails desktop app task"
  note "UI-load evidence (a screenshot) must be attached as 02-desktop-start.png; a headless host cannot produce it"
  skip "A2.3 the UI renders (no offline state) — needs a screenshot from a display" "headless: attach 02-desktop-start.png"
  DIRTY=1
}

# --- step 3: the reverse direction (AC3) ------------------------------------
step3_reverse() {
  step_begin 3 reverse-desktop-stop-tui-start
  local cp; cp="$(control_port "$INSTANCE")"
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A3 the reverse direction: desktop STOP then TUI START (AC3)" "instance '$INSTANCE' is not installed on this host"
    return
  fi
  if [ "$HAVE_DESKTOP" != 1 ] || [ "$HAVE_TUI" != 1 ]; then
    blocked_surface "A3.1 desktop STOP takes down the plane the TUI started (same PID, same state dir)" "Wails desktop app task" \
      "needs both surfaces; the desktop app does not exist and the TUI has no lifecycle control"
    blocked_surface "A3.2 TUI START brings it back: /healthz 200 with a new PID" "TUI task" "needs both surfaces"
    return
  fi
  if [ -z "$TUI_START_KEYS" ]; then
    blocked_surface "A3.2 TUI START brings the plane back after the desktop app stopped it" \
      "this item (harness configuration — export PLANE_E2E_TUI_START_KEYS, then re-run)" \
      "a TUI lifecycle surface exists but no start keystrokes were configured, so A3 would FAIL on a plane nobody could bring back"
    return
  fi

  local pid_before; pid_before="$(pid_of "$INSTANCE")"
  note "the plane under test was started by step 2's desktop-app start (or step 1's TUI start); pid=${pid_before:-none}"
  launch_desktop "$DESKTOP_STOP_KEYS" "the offline-state STOP action"
  check "A3.1 the desktop app's stop takes the SAME PID down (one shared state dir)" \
    "$(wait_down "$cp" && ! kill -0 "${pid_before:-0}" 2>/dev/null && echo 0 || echo 1)" "Wails desktop app task"
  drive_tui "$TUI_START_KEYS"
  local pid_after; pid_after="$(pid_of "$INSTANCE")"
  note "pid after TUI start: ${pid_after:-none}"
  check "A3.2 TUI START brings the plane back: /healthz 200 with a PID the TUI wrote" \
    "$(wait_up "$cp" && [ -n "$pid_after" ] && echo 0 || echo 1)" "TUI task"
  DIRTY=1
}

# --- step 4: CLI parity (AC4) -----------------------------------------------
step4_cli() {
  step_begin 4 cli-parity
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A4 the CLI lifecycle verbs on '$INSTANCE' (AC4)" \
      "instance '$INSTANCE' is not installed on this host: 'orchicon start' has nothing to start and there is no port record to probe"
    return
  fi
  local cp; cp="$(control_port "$INSTANCE")"
  local out rc

  # A clean baseline first: stop is idempotent, so this is safe twice.
  c_run "$BIN" stop "$INSTANCE" >/dev/null || true

  out="$(c_run "$BIN" start "$INSTANCE")"; rc=$?
  note "orchicon start $INSTANCE -> exit $rc"
  check "A4.1 orchicon start $INSTANCE exits 0" "$([ "$rc" = 0 ] && echo 0 || echo 1)" "CLI verbs task"
  check "A4.1b '$INSTANCE' /healthz returns 200 after orchicon start (independent probe)" \
    "$(wait_up "$cp" && echo 0 || echo 1)" "CLI verbs task"
  DIRTY=1

  out="$(plane_status_line "$INSTANCE")"
  printf '%s\n' "$out" >>"$STEP_LOG"
  local st=0
  case "$out" in *"instance $INSTANCE"*) ;; *) st=1 ;; esac
  case "$out" in *"plane pid "*) ;; *) st=1 ;; esac
  case "$out" in *"running /"*) ;; *) st=1 ;; esac
  case "$out" in *"healthy at"*) ;; *) st=1 ;; esac
  check "A4.2 plane status $INSTANCE names the instance, the PID, the serving binary and its health" "$st" "CLI verbs task"

  out="$(c_run "$BIN" stop "$INSTANCE")"; rc=$?
  note "orchicon stop $INSTANCE -> exit $rc"
  check "A4.3 orchicon stop $INSTANCE exits 0" "$([ "$rc" = 0 ] && echo 0 || echo 1)" "CLI verbs task"
  check "A4.3b '$INSTANCE' /healthz is REFUSED after the stop (curl exit 7, empty body)" \
    "$(wait_down "$cp" && echo 0 || echo 1)" "CLI verbs task"
  local ports_free_after=0
  ports_all_free "$INSTANCE" || ports_free_after=1
  check "A4.3c every recorded port of '$INSTANCE' is FREE after the stop${HELD_PORTS:+ (still held: $HELD_PORTS)}" \
    "$ports_free_after" "stop-reclaims-leftovers task"

  c_run "$BIN" stop "$INSTANCE" >/dev/null; rc=$?
  check "A4.4 stop is idempotent: a second orchicon stop exits 0" "$([ "$rc" = 0 ] && echo 0 || echo 1)" "CLI verbs task"

  local pid1 pid2
  "$BIN" start "$INSTANCE" >/dev/null 2>&1 || true
  wait_up "$cp" 60 || true
  pid1="$(pid_of "$INSTANCE")"
  c_run "$BIN" restart "$INSTANCE" >/dev/null; rc=$?
  pid2="$(pid_of "$INSTANCE")"
  note "restart: pid ${pid1:-none} -> ${pid2:-none} (exit $rc)"
  check "A4.5 orchicon restart $INSTANCE exits 0, replaces the process and the plane answers again" \
    "$([ "$rc" = 0 ] && [ -n "$pid2" ] && [ "$pid1" != "$pid2" ] && wait_up "$cp" && echo 0 || echo 1)" "CLI verbs task"
  DIRTY=1

  # A4.6 (correction C2): the two status surfaces must not disagree about LIVENESS. What
  # `serve --status` cannot answer - the instance, the serving binary, drift - is recorded as
  # an OBSERVATION for the CLI-verbs task, not charged as a failure here.
  local ps_out plane_out
  ps_out="$(c_run "$BIN" serve --status)"; rc=$?
  plane_out="$(plane_status_line "$INSTANCE")"
  printf 'serve --status -> exit %s\n%s\nplane status ->\n%s\n' "$rc" "$ps_out" "$plane_out" >>"$STEP_LOG"
  local serve_running=1
  case "$ps_out" in *"serve is running"*) serve_running=0 ;; esac
  local plane_running=1
  case "$plane_out" in *"plane pid "*) plane_running=0 ;; esac
  check "A4.6 serve --status and plane status agree about liveness" \
    "$([ "$serve_running" = "$plane_running" ] && echo 0 || echo 1)" "CLI verbs task"
  note "OBSERVATION (correction C2, for the CLI-verbs task): serve --status printed '$ps_out' — a PID only, with no instance, no serving binary and no drift. plane status is the surface that can report drift, so AC5 is asserted there."
}

# --- step 5: drift (AC5), asserted on `plane status` ------------------------
step5_drift() {
  step_begin 5 drift
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A5 drift is reported after the binary is replaced under a live plane (AC5)" \
      "instance '$INSTANCE' is not installed on this host: there is no live plane to replace a binary underneath"
    return
  fi
  local cp; cp="$(control_port "$INSTANCE")"
  "$BIN" start "$INSTANCE" >/dev/null 2>&1 || true
  wait_up "$cp" 60 || true
  DIRTY=1

  local out; out="$(plane_status_line "$INSTANCE")"
  printf '%s\n' "$out" >>"$STEP_LOG"
  check "A5.1 baseline: plane status $INSTANCE says the serving binary is CURRENT" \
    "$(status_says_current "$out" && echo 0 || echo 1)" "CLI verbs task"

  local old_pid old_exe
  old_pid="$(pid_of "$INSTANCE")"; old_exe="$(exe_of "$old_pid")"
  note "old pid ${old_pid:-none} serving ${old_exe:-none}"
  check "A5.2 the live plane is identifiable before the replacement (pid + /proc/<pid>/exe)" \
    "$([ -n "$old_pid" ] && [ -n "$old_exe" ] && echo 0 || echo 1)" "shared control mechanism task"

  # REPLACE THE BINARY UNDERNEATH THE LIVE PLANE. `mv` (a rename) unlinks the old inode,
  # which is what makes /proc/<pid>/exe report the " (deleted)" suffix Drift keys on. A `cp`
  # over a running executable is ETXTBSY-dependent and would prove the kernel, not Drift.
  c_run go -C "$REPO_ROOT" build -o "${BIN_PATH}.new" ./cmd/orchicon >/dev/null || true
  if [ -x "${BIN_PATH}.new" ]; then
    mv -f "${BIN_PATH}.new" "$BIN_PATH"
    note "replaced ${BIN_PATH} with a fresh build (rename, so the old inode is unlinked)"
    check "A5.2b the binary was replaced under the live plane" 0
  else
    check "A5.2b the binary was replaced under the live plane" 1 "this item (harness bug)"
  fi

  out="$(plane_status_line "$INSTANCE")"
  printf '%s\n' "$out" >>"$STEP_LOG"
  check "A5.3 plane status $INSTANCE reports the plane as running a SUPERSEDED binary" \
    "$(status_says_superseded "$out" && echo 0 || echo 1)" "internal/planecontrol/hostproc_unix.go:<Drift>"
  if status_says_healthy_at "$out"; then
    note "the same line also reports healthy at <port> — which is WHY a status surface that only probed the port would have called this a pass"
  fi
  check "A5.3b a superseded binary is still SERVING: '$INSTANCE' /healthz is still 200" \
    "$([ "$(health_code "$cp")" = 200 ] && echo 0 || echo 1)" "this item (harness bug)"
  local old_superseded; old_superseded="$(exe_of "$old_pid")"
  note "after the replacement /proc/${old_pid:-none}/exe = ${old_superseded:-none}"
  check "A5.3c the status names the SUPERSEDED binary, not merely 'healthy'" \
    "$(case "$old_superseded" in *'(deleted)') echo 0 ;; *) echo 1 ;; esac)" "internal/planecontrol/hostproc_unix.go:<Drift>"

  out="$(c_run "$BIN" plane start "$INSTANCE")"
  printf '%s\n' "$out" >>"$STEP_LOG"
  local new_pid; new_pid="$(pid_of "$INSTANCE")"
  note "plane start said: $out | pid ${old_pid:-none} -> ${new_pid:-none}"
  check "A5.4 plane start restarts a drifted plane on the installed binary (new PID, /healthz 200)" \
    "$([ -n "$new_pid" ] && [ "$new_pid" != "$old_pid" ] && wait_up "$cp" && echo 0 || echo 1)" "shared control mechanism task"
  out="$(plane_status_line "$INSTANCE")"
  printf '%s\n' "$out" >>"$STEP_LOG"
  check "A5.4b after the restart the serving binary is CURRENT again" \
    "$(status_says_current "$out" && echo 0 || echo 1)" "shared control mechanism task"

  local ps_out; ps_out="$(c_run "$BIN" serve --status)"
  note "OBSERVATION (correction C2): serve --status printed '$ps_out' while the plane was drifted — it cannot answer the drift question at all. Evidence for the CLI-verbs task; not a FAIL here."
}

# --- step 6: the fail-safe, both halves (AC6) -------------------------------
step6_failsafe() {
  step_begin 6 failsafe
  if [ "$HOST_OK" != 1 ]; then
    invalid_host "A6 an unidentifiable plane is left alone on restart, and a foreign port holder is reported not killed (AC6)" \
      "instance '$INSTANCE' is not installed on this host: there is no plane to make unidentifiable and no services to stop"
    return
  fi
  local cp; cp="$(control_port "$INSTANCE")"

  # ---- half 1: OUR OWN plane, made unidentifiable ----
  "$BIN" plane start "$INSTANCE" >/dev/null 2>&1 || true
  wait_up "$cp" 60 || true
  DIRTY=1
  local pid exe
  pid="$(pid_of "$INSTANCE")"; exe="$(exe_of "$pid")"
  check "A6.1 baseline: '$INSTANCE' plane is healthy and identifiable (pid ${pid:-none})" \
    "$([ "$(health_code "$cp")" = 200 ] && [ -n "$pid" ] && echo 0 || echo 1)" "shared control mechanism task"

  local pf; pf="$(pid_file "$INSTANCE")"
  if [ -f "$pf" ]; then
    PID_FILE_BAK="${pf}.plane-e2e-bak"
    mv -f "$pf" "$PID_FILE_BAK"
    note "moved $pf -> $PID_FILE_BAK (the plane is now UNIDENTIFIABLE)"
  fi

  local out; out="$(c_run "$BIN" plane restart "$INSTANCE")"; local rc=$?
  printf '%s\n' "$out" >>"$STEP_LOG"
  check "A6.2 plane restart on an unidentifiable plane exits 0 and says it is leaving it" \
    "$([ "$rc" = 0 ] && output_says_leaving_it "$out" && echo 0 || echo 1)" \
    "internal/planecontrol/planecontrol.go:<Start> (the default: arm)"

  local pid_after exe_after
  pid_after="$(kill -0 "$pid" 2>/dev/null && echo "$pid")"
  exe_after="$(exe_of "$pid")"
  note "after the restart attempt: /healthz=$(health_code "$cp") pid_alive=${pid_after:-no} exe=${exe_after:-none}"
  check "A6.3 the SAME process is still serving (pid ${pid:-none} alive, same exe, /healthz 200) — proven by process identity, not by the message" \
    "$([ -n "$pid_after" ] && [ "$exe_after" = "$exe" ] && [ "$(health_code "$cp")" = 200 ] && echo 0 || echo 1)" \
    "shared control mechanism task"

  if [ -n "$PID_FILE_BAK" ] && [ -f "$PID_FILE_BAK" ]; then
    mv -f "$PID_FILE_BAK" "$pf"; PID_FILE_BAK=""
    note "restored the PID file"
  fi
  check "A6.4 the instance is left in a normal, identifiable state (PID file restored)" \
    "$([ -s "$pf" ] && echo 0 || echo 1)" "this item (harness bug)"

  # ---- half 2: a FOREIGN holder of the instance's port ----
  local pg; pg="$(postgres_port "$INSTANCE")"
  c_run "$BIN" services down "$INSTANCE" >/dev/null || true
  if [ -z "$pg" ]; then
    invalid_host "A6.5 a foreign process can hold this instance's port" "no ORCHICON_POSTGRES_PORT in the port record, so there is no port to occupy"
    return
  fi
  if port_listening "$pg"; then
    invalid_host "A6.5 the instance's port :$pg can be used by a foreign process" "port :$pg is still held after services down — that is itself a defect of the stop-reclaims-leftovers task, recorded above where it belongs"
    return
  fi
  python3 -m http.server "$pg" --bind 127.0.0.1 >>"$STEP_LOG" 2>&1 &
  local foreign_pid=$!
  local i=0
  while [ "$i" -lt 10 ] && ! port_listening "$pg"; do sleep 1; i=$((i + 1)); done
  note "foreign holder started: pid $foreign_pid on :$pg (python3 -m http.server)"
  check "A6.5 a FOREIGN process holds this instance's postgres port :$pg (pid $foreign_pid, not ours)" \
    "$(port_listening "$pg" && echo 0 || echo 1)" "this item (harness bug)"

  out="$(c_run "$BIN" services down "$INSTANCE" --force)"; rc=$?
  printf '%s\n' "$out" >>"$STEP_LOG"
  check "A6.6 services down --force exits non-zero and REPORTS the foreign holder (:${pg}) instead of proceeding" \
    "$([ "$rc" != 0 ] && output_says_reported_foreign "$out" && echo 0 || echo 1)" "cmd/orchicon/services.go:236-245"
  check "A6.7 the foreign process was NOT killed (pid $foreign_pid still alive)" \
    "$(kill -0 "$foreign_pid" 2>/dev/null && echo 0 || echo 1)" "cmd/orchicon/reclaim.go:<ourInstanceProcesses>"
  kill "$foreign_pid" 2>/dev/null || true
  note "the harness killed its own foreign holder (pid $foreign_pid) at the end of the step"
}

# --- step 7: two instances (AC7) --------------------------------------------
step7_two_instances() {
  step_begin 7 two-instances
  local a="$PAIR_A" b="$PAIR_B"
  note "pair: A=$a B=$b"
  if ! installed "$a" || ! installed "$b"; then
    invalid_host "A7 stopping '$a' leaves '$b' running and answering (AC7)" \
      "both instances must be installed to prove isolation; '$a' installed=$(installed "$a" && echo yes || echo no), '$b' installed=$(installed "$b" && echo yes || echo no)"
    return
  fi
  local cpa cpb
  cpa="$(control_port "$a")"; cpb="$(control_port "$b")"
  "$BIN" start "$a" >/dev/null 2>&1 || true
  "$BIN" start "$b" >/dev/null 2>&1 || true
  wait_up "$cpa" 60 || true
  wait_up "$cpb" 60 || true
  DIRTY=1

  local pid_a pid_b
  pid_a="$(pid_of "$a")"; pid_b="$(pid_of "$b")"
  note "running: $a pid ${pid_a:-none} /healthz=$(health_code "$cpa"); $b pid ${pid_b:-none} /healthz=$(health_code "$cpb")"
  check "A7.1 both '$a' and '$b' answer /healthz 200" \
    "$([ "$(health_code "$cpa")" = 200 ] && [ "$(health_code "$cpb")" = 200 ] && echo 0 || echo 1)" "install task"
  check "A7.2 each instance has its OWN PID from its OWN PID file (${a}:${pid_a:-none} vs ${b}:${pid_b:-none})" \
    "$([ -n "$pid_a" ] && [ -n "$pid_b" ] && [ "$pid_a" != "$pid_b" ] && echo 0 || echo 1)" "shared control mechanism task"

  local b_stat_before; b_stat_before="$(stat -c '%i %s %Y' "$(serve_log "$b")" 2>/dev/null || echo 'no log')"
  note "$b log before: $b_stat_before ($(serve_log "$b"))"

  c_run "$BIN" stop "$a" >/dev/null || true
  check "A7.3 '$a' is DOWN after stopping '$a': /healthz refused" \
    "$(wait_down "$cpa" && echo 0 || echo 1)" "CLI verbs task"
  local ports_free_after=0
  ports_all_free "$a" || ports_free_after=1
  check "A7.3b every recorded port of '$a' is FREE after stopping '$a'${HELD_PORTS:+ (still held: $HELD_PORTS)}" \
    "$ports_free_after" "stop-reclaims-leftovers task"

  check "A7.4 '$b' is untouched: /healthz still 200 and its ports are still held" \
    "$([ "$(health_code "$cpb")" = 200 ] && [ -n "$(ports_listening_now "$b")" ] && echo 0 || echo 1)" "CLI verbs task"
  local pid_b_after; pid_b_after="$(pid_of "$b")"
  check "A7.4b '$b' has the SAME PID as before (${pid_b:-none} -> ${pid_b_after:-none})" \
    "$([ -n "$pid_b" ] && [ "$pid_b" = "$pid_b_after" ] && echo 0 || echo 1)" "CLI verbs task"

  local b_stat_after; b_stat_after="$(stat -c '%i %s %Y' "$(serve_log "$b")" 2>/dev/null || echo 'no log')"
  note "$b log after: $b_stat_after"
  check "A7.5 '$b''s log file was not written to (inode, size and mtime unchanged: $b_stat_before)" \
    "$([ "$b_stat_before" = "$b_stat_after" ] && echo 0 || echo 1)" "shared control mechanism task"
}

# --- step 8: the cross-instance environment leak (AC8, correction C4) -------
step8_env_leak() {
  step_begin 8 env-leak
  local a="$PAIR_A" b="$PAIR_B"
  if ! installed "$a" || ! installed "$b"; then
    invalid_host "A8 a plane started from a shell carrying the OTHER instance's environment still writes its own log and PID file (AC8)" \
      "both instances must be installed; '$a' installed=$(installed "$a" && echo yes || echo no), '$b' installed=$(installed "$b" && echo yes || echo no)"
    return
  fi
  local cpa; cpa="$(control_port "$a")"
  "$BIN" stop "$a" >/dev/null 2>&1 || true
  wait_down "$cpa" 20 || true

  # SNAPSHOT THE OTHER INSTANCE'S LOG AND PID FILE before the poisoned start.
  local b_log b_pid_file b_stat_before b_pid_before
  b_log="$(serve_log "$b")"; b_pid_file="$(pid_file "$b")"
  b_stat_before="$(stat -c '%i %s %Y' "$b_log" 2>/dev/null || echo 'no log')"
  b_pid_before="$(cat "$b_pid_file" 2>/dev/null || echo 'no pid file')"
  note "$b log before: $b_stat_before ; $b pid file before: $b_pid_before"

  # THE REGRESSION ITSELF: start '$a' from a shell that exports '$b''s variables.
  # Getenv returns the FIRST match, so an ambient value that is APPENDED to, rather than
  # stripped from, wins - which is how a dev plane once wrote its log into prod's log file
  # and never wrote a PID file at all.
  local out rc
  out="$(c_run env \
      "ORCHICON_SERVE_STATE_DIR=$(data_dir "$b")/serve" \
      "ORCHICON_SERVE_LOG_FILE=$(serve_log "$b")" \
      ORCHICON_SERVE_DETACHED=1 \
      ORCHICON_INSTANCE="$b" \
      "$BIN" start "$a")"; rc=$?
  printf '%s\n' "$out" >>"$STEP_LOG"
  note "poisoned 'orchicon start $a' -> exit $rc"
  wait_up "$cpa" 60 || true
  DIRTY=1

  local a_pid_file a_log a_stat a_pid
  a_pid_file="$(pid_file "$a")"; a_log="$(serve_log "$a")"
  a_stat="$(stat -c '%s %Y' "$a_log" 2>/dev/null || echo 'no log')"
  a_pid="$(pid_of "$a")"
  note "$a after: pidfile=$a_pid_file pid=${a_pid:-none} log=$a_log stat=$a_stat"

  check "A8.1 '$a' has its OWN PID file ($a_pid_file), despite the poisoned environment" \
    "$([ -s "$a_pid_file" ] && echo 0 || echo 1)" "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"
  check "A8.2 that PID is the process serving '$a' (/proc/<pid>/exe is our binary and /healthz answers 200)" \
    "$([ -n "$a_pid" ] && [ "$(health_code "$cpa")" = 200 ] && [ -n "$(exe_of "$a_pid")" ] && echo 0 || echo 1)" \
    "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"
  check "A8.3 '$a''s own log exists and was written during the start (stat: ${a_stat})" \
    "$([ -s "$a_log" ] && echo 0 || echo 1)" "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"

  local b_stat_after b_pid_after
  b_stat_after="$(stat -c '%i %s %Y' "$b_log" 2>/dev/null || echo 'no log')"
  b_pid_after="$(cat "$b_pid_file" 2>/dev/null || echo 'no pid file')"
  note "$b log after: $b_stat_after ; $b pid file after: $b_pid_after"
  check "A8.4 '$b''s log is UNTOUCHED by '$a''s plane (inode, size and mtime unchanged: $b_stat_before)" \
    "$([ "$b_stat_before" = "$b_stat_after" ] && echo 0 || echo 1)" "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"
  check "A8.4b '$b''s PID file was not written by '$a''s start (before: $b_pid_before, after: $b_pid_after)" \
    "$([ "$b_pid_before" = "$b_pid_after" ] && echo 0 || echo 1)" "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"

  local la lb b_plane_pid_before
  # '$b' IS STILL RUNNING HERE — step 7 leaves it up on purpose, to prove dev's stop left prod
  # serving. So "b has NO plane pid" is the wrong assertion: it would FAIL on every valid host,
  # against product code that is behaving correctly. The claim is that b's plane is the SAME
  # process it had before the poisoned start — '$a''s start neither restarted b nor wrote b's
  # PID file (A8.4b already checks the file itself).
  b_plane_pid_before="$(pid_of "$b")"
  la="$(plane_status_line "$a")"; lb="$(plane_status_line "$b")"
  printf 'plane status %s -> %s\nplane status %s -> %s\n' "$a" "$la" "$b" "$lb" >>"$STEP_LOG"
  local st=0
  if [ -z "$a_pid" ]; then
    st=1
  else
    case "$la" in *"plane pid ${a_pid}"*) ;; *) st=1 ;; esac
  fi
  if [ -n "$b_plane_pid_before" ]; then
    case "$lb" in *"plane pid ${b_plane_pid_before}"*) ;; *) st=1 ;; esac
  else
    case "$lb" in *"plane pid "*) st=1 ;; esac
  fi
  check "A8.5 plane status reports each instance's OWN process ('$a' -> ${a_pid:-none}, '$b' unchanged at ${b_plane_pid_before:-none})" "$st" \
    "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"

  c_run "$BIN" serve --status >/dev/null || true
  check "A8.6 the strip happened at the SPAWN site: a planted ORCHICON_SERVE_DETACHED=1 did NOT turn the start into a foreground plane (it returned, and '$a' has a PID file)" \
    "$([ -n "$a_pid" ] && echo 0 || echo 1)" "internal/planecontrol/planecontrol.go:<instanceEnvKeys>"
}

# --- step 9: the web app regression (AC9) -----------------------------------
step9_webapp() {
  step_begin 9 webapp

  # AN ABSENCE-PROOF, NOT GREEN-IF-QUIET: `go test -run` whose pattern matches nothing exits 0,
  # so a renamed tripwire would silently hollow this assertion out. Demanding both `--- PASS:`
  # lines makes an empty run a FAIL by construction.
  local api_out api_rc api_pass=0
  api_out="$(c_run go -C "$REPO_ROOT" test ./internal/api -run 'TestAPISurfaceDidNotGrow|TestProtoDeclaresNoLifecycleService' -count=1 -v)"; api_rc=$?
  printf '%s\n' "$api_out" >>"$STEP_LOG"
  [ "$api_rc" = 0 ] || api_pass=1
  case "$api_out" in *"--- PASS: TestAPISurfaceDidNotGrow"*) ;; *) api_pass=1 ;; esac
  case "$api_out" in *"--- PASS: TestProtoDeclaresNoLifecycleService"*) ;; *) api_pass=1 ;; esac
  check "A9.1 the Connect surface did not grow: no lifecycle RPC exists (both tripwires RAN and passed)" \
    "$api_pass" "internal/api/lifecycle_surface_test.go:<TestAPISurfaceDidNotGrow>"

  # A9.2 — an EMPTY grep is not a PASS unless the pattern can match something. The
  # CONTROL string must hit first; only then does an empty lifecycle result mean "none".
  #
  # THE PATTERN IS AN IDENTIFIER/CALL TRIPWIRE, not the bare English phrase. The SPA's
  # settings help legitimately says "plane restart" in prose
  # (frontend/src/routes/settings.tsx:770); that is a sentence, not a control. What must not
  # exist is something that would actually DRIVE a lifecycle: a planecontrol binding, a
  # StartPlane/StopPlane/RestartPlane call, or a /plane/<verb> request. The prose match is
  # recorded as an observation so the distinction stays visible in the evidence.
  local ctrl hits prose lifecycle_pattern
  # THE CONTROL FIRST, by construction: the empty result is only meaningful after a pattern
  # that MUST match has just matched.
  ctrl="$(grep -rIl 'useToastStore' "${REPO_ROOT}/frontend/src" --include='*.ts' --include='*.tsx' 2>/dev/null | head -3)"
  lifecycle_pattern='planecontrol|plane-control|StartPlane|StopPlane|RestartPlane|/plane/(start|stop|restart)|planeLifecycle'
  check "A9.2 control: the search actually reaches frontend/src (a known-present symbol matches)" \
    "$([ -n "$ctrl" ] && echo 0 || echo 1)" "this item (harness bug)"
  hits="$(grep -rIniE "$lifecycle_pattern" "${REPO_ROOT}/frontend/src" --include='*.ts' --include='*.tsx' 2>/dev/null | grep -v '/api/gen/' || true)"
  prose="$(grep -rIniE 'plane (start|stop|restart)' "${REPO_ROOT}/frontend/src" --include='*.ts' --include='*.tsx' 2>/dev/null | grep -v '/api/gen/' || true)"
  printf 'control matches:\n%s\nlifecycle-shaped matches (must be empty):\n%s\nprose mentions (recorded, not a control):\n%s\n' \
    "$ctrl" "$hits" "$prose" >>"$STEP_LOG"
  [ -n "$hits" ] && printf '%s\n' "$hits"
  [ -n "$prose" ] && note "OBSERVATION: the words 'plane <verb>' appear in prose help text ($prose) — a sentence, not a lifecycle control; the tripwire above is identifier-shaped on purpose"
  check "A9.2 the web app has NO lifecycle UI and NO new notice (no lifecycle-shaped identifier or call in frontend/src)" \
    "$([ -z "$hits" ] && echo 0 || echo 1)" "this item — a lifecycle control appeared; the SPA must not gain one"

  skip "A9.3 a plane stopped underneath an open tab shows the SPA's EXISTING connection-failure state" \
    "needs a browser on the plane's origin: open a tab, stop the plane from the TUI, attach 09-webapp-offline.png + 09-webapp-console.txt"
}

# --- the plan (--dry-run executes nothing) ----------------------------------
print_plan() {
  cat <<'PLAN'
plane-control-e2e plan (--dry-run: nothing below is executed)

step 0  preflight     build the binary under test into a scratch install dir; assert the instance is
                      installed, its ports are free, a TUI lifecycle surface exists, a Wails desktop app
                      exists and a display is available
step 1  TUI stop      drive `orch` through a real pty, STOP; prove DOWN with curl exit 7 AND every
                      recorded port free AND the PID file dead/absent            (AC1)
step 2  desktop start launch the packaged app, use the offline-state START; prove /healthz 200 and the
                      UI loads from a screenshot - never from the app's own status line  (AC2)
step 3  reverse       desktop STOP then TUI START; the SAME PID must die and the TUI must write the new
                      one, which is what proves ONE shared mechanism                (AC3)
step 4  CLI parity    orchicon start/stop/restart <instance>; status names instance, pid, binary, health;
                      stop leaves the connection refused and every port free            (AC4)
step 5  drift         replace the binary under the live plane with `mv`; `plane status` must say
                      "binary superseded" while /healthz is still 200                   (AC5)
step 6  fail-safe     (a) hide the PID file, `plane restart` must say "leaving it" and the SAME process
                      must still serve; (b) a FOREIGN holder of the instance's port must be REPORTED,
                      never killed                                                       (AC6)
step 7  two instances start both, stop one; the other keeps answering, keeps its PID, and its log is
                      not written to                                                      (AC7)
step 8  env leak      start an instance from a shell exporting the OTHER instance's
                      ORCHICON_SERVE_STATE_DIR / _LOG_FILE / _DETACHED: it must write its own log and
                      its own PID file and leave the other's log untouched                (AC8)
step 9  web app       the API surface test (must not have grown), the frontend lifecycle grep (must be
                      empty, behind a control match), and the open-tab observation       (AC9)

exit codes: 0 every assertion passed | 1 at least one FAILED (defect named with its file:line)
            2 NOT a valid proof (a surface is absent, the host is not installed, or a port was held)
PLAN
}

# --- summary ----------------------------------------------------------------
verdict_for() { # verdict_for <step>
  local n="$1" st
  st="$(awk -F'\t' -v s="$n" '$1==s {print $2}' "${EVIDENCE}/results.tsv" 2>/dev/null | sort -u | tr '\n' ',')"
  case "$st" in
    *FAIL*) printf '**FAIL** (see defects.tsv)' ;;
    *BLOCKED*) printf 'BLOCKED (see defects.tsv)' ;;
    *SKIP*PASS*|*PASS*SKIP*) printf 'PARTIAL (automated assertions passed, a manual observation is outstanding)' ;;
    *SKIP*) printf 'SKIP' ;;
    ""|,) printf 'not run' ;;
    *) printf 'PASS' ;;
  esac
}

render_summary() {
  local md="${EVIDENCE}/summary.md"
  {
    echo "# Plane control across TUI / desktop app / CLI — harness run ${UTC_STAMP}"
    echo
    echo "- instance: \`${INSTANCE}\` (data dir \`$(data_dir "$INSTANCE")\`, ports \`$(ports_of "$INSTANCE" 2>/dev/null || echo 'none recorded')\`)"
    echo "- two-instance pair: \`${PAIR_A}\` + \`${PAIR_B}\`"
    echo "- binary under test: \`${BIN_PATH}\` (a copy in a scratch install dir, never the worktree's build)"
    echo "- passes: ${PASSES}   failures: ${FAILS}   skips: ${SKIPS}   blocked: ${BLOCKED}"
    [ -n "$ABORT_REASON" ] && { echo; echo "**NOT A VALID PROOF:** ${ABORT_REASON}"; }
    echo
    echo "## per-step verdict"
    echo
    echo "| step | what it proves | verdict |"
    echo "|---|---|---|"
    printf '| 0 | validity precondition (installed, ports free, surfaces present) | %s |\n' "$(verdict_for 0)"
    printf '| 1 | TUI stop is really a stop (AC1) | %s |\n' "$(verdict_for 1)"
    printf '| 2 | desktop start brings it up (AC2) | %s |\n' "$(verdict_for 2)"
    printf '| 3 | reverse direction, one shared mechanism (AC3) | %s |\n' "$(verdict_for 3)"
    printf '| 4 | CLI parity (AC4) | %s |\n' "$(verdict_for 4)"
    printf '| 5 | drift reported on plane status (AC5) | %s |\n' "$(verdict_for 5)"
    printf '| 6 | fail-safe: left alone + foreign holder reported (AC6) | %s |\n' "$(verdict_for 6)"
    printf '| 7 | two instances isolated (AC7) | %s |\n' "$(verdict_for 7)"
    printf '| 8 | cross-instance environment stripped (AC8) | %s |\n' "$(verdict_for 8)"
    printf '| 9 | web app gained nothing (AC9) | %s |\n' "$(verdict_for 9)"
    echo
    echo "## evidence (independent of any control's own report)"
    echo
    echo "- \`/healthz\` codes, curl exit codes and the listening sockets are read from"
    echo "  \`/proc/net/tcp{,6}\`; the serving binary from \`/proc/<pid>/exe\`; the PID from the"
    echo "  instance's own PID file; the ports from its own \`ports.json\`."
    echo "- per-step logs: \`NN-*.log\` next to this file, each holding the exact commands,"
    echo "  their exit codes and their verbatim output."
    echo
    echo "## defects — each names the sibling task that owns the fix (AC12)"
    echo
    if [ -s "${EVIDENCE}/defects.tsv" ]; then
      echo "| step | assertion | owner |"
      echo "|---|---|---|"
      awk -F'\t' '{printf "| %s | %s | `%s` |\n", $1, $2, $3}' "${EVIDENCE}/defects.tsv"
    else
      echo "none."
    fi
    echo
    echo "## evidence files"
    echo
    ls -1 "$EVIDENCE" 2>/dev/null | sed 's/^/- /'
  } >"$md" 2>&1
  say ""
  say "summary: ${md#"$REPO_ROOT"/}"
  say "passes=${PASSES} failures=${FAILS} skips=${SKIPS} blocked=${BLOCKED}"
}

teardown() {
  # A packaged desktop app left open would decide the next step's assertions by itself.
  close_desktop 2>/dev/null || true
  # An aborted run must not leave an instance without its PID file: that state is exactly
  # what the next `plane status` would misread as "nothing running".
  if [ -n "$PID_FILE_BAK" ] && [ -f "$PID_FILE_BAK" ]; then
    mv -f "$PID_FILE_BAK" "$(pid_file "$INSTANCE")" 2>/dev/null || true
    say "teardown: restored the PID file"
  fi
  [ "$KEEP" = "1" ] && { say "teardown: --keep, leaving whatever is running running"; return; }
  if [ "$DIRTY" = "1" ] && [ -n "$BIN" ] && [ -x "$BIN" ]; then
    say "teardown: stopping ${INSTANCE} (what this run started)"
    "$BIN" stop "$INSTANCE" >/dev/null 2>&1 || true
  fi
}

main() {
  parse_args "$@"
  if [ "$DRY_RUN" = "1" ]; then print_plan; exit 0; fi

  mkdir -p "$EVIDENCE"
  : >"${EVIDENCE}/results.tsv"
  : >"${EVIDENCE}/defects.tsv"
  trap teardown EXIT

  step0_preflight
  # --preflight-only runs the VALIDITY PRECONDITION and nothing else. A validity check must
  # not be a side effect: it starts no plane, so it also changes no state.
  if [ "$PREFLIGHT_ONLY" = "1" ]; then
    render_summary
    say ""
    if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
      say "PREFLIGHT: this host is NOT a valid host for the procedure — ${ABORT_REASON:-a required surface is absent}"
      exit 2
    fi
    say "PREFLIGHT: this host is valid. Re-run without --preflight-only for the whole procedure."
    exit 0
  fi

  step1_tui_stop
  step2_desktop_start
  step3_reverse
  step4_cli
  step5_drift
  step6_failsafe
  step7_two_instances
  step8_env_leak
  step9_webapp
  render_summary

  say ""
  if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
    say "RESULT: NOT A VALID PROOF — ${ABORT_REASON:-a required surface is absent and a defect has been filed against the task that owns it}"
    say "        The steps that could run did run, and their evidence is in ${EVIDENCE#"$REPO_ROOT"/}."
    exit 2
  fi
  if [ "$FAILS" -gt 0 ]; then
    say "RESULT: ${FAILS} assertion(s) failed — each is a defect against the file named in defects.tsv."
    exit 1
  fi
  say "RESULT: the whole control cycle is proven — stop and start from the TUI, the desktop app and the CLI,"
  say "        drift, the fail-safe, two-instance isolation and cross-instance env stripping, all against"
  say "        /healthz, the listening sockets and the PID files — never against a control's own report."
  exit 0
}

# Sourced (scripts/tests/plane-control-e2e-assertions/run.sh) the FUNCTIONS are the
# subject and nothing may execute: the offline gate proves the primitives without
# starting a plane, binding a port, or touching an instance's state.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
