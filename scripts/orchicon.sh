#!/usr/bin/env bash
#
# orchicon.sh — START/STOP THE ORCHICON PLANE AND ITS SERVICES ON THE HOST.
#
# WHY THIS EXISTS. This is the day-to-day entry point: `start`, `stop`,
# `restart`, `status`, `logs`, `rebuild`, per instance. It exists so a developer
# never has to remember which of the plane's two halves (the SERVICES — postgres,
# nats, telemetry — and the PLANE itself) is which, or which command reaches it.
#
# USAGE
#   scripts/orchicon.sh start   [dev|prod]   # services + plane for one instance
#   scripts/orchicon.sh stop    [dev|prod]   # stop both halves
#   scripts/orchicon.sh restart [dev|prod]
#   scripts/orchicon.sh status  [dev|prod]   # what is up, and is it really healthy
#   scripts/orchicon.sh logs    [dev|prod]   # the plane's log (follows when a tty)
#   scripts/orchicon.sh rebuild [dev|prod]   # rebuild + restart (quick path; see below)
#   scripts/orchicon.sh help
#
# INSTANCES. `dev` listens on 127.0.0.1:8080, `prod` on 127.0.0.1:8091. They are
# fully separate — separate data dirs (~/.local/share/orchicon-<instance>),
# separate databases, separate ports — so stopping or restarting one never
# touches the other. Default: dev. Anything else is refused (exit 2).
#
# CLIENT
#   The TUI talks to an instance through a profile in ~/.orchicon/config:
#     /connect          pick or add a profile (url + token), then
#     orch              start the TUI
#   The GUI is served BY the plane: open http://127.0.0.1:8080 (dev) or
#   http://127.0.0.1:8091 (prod).
#
# SHAPE. Services and plane are HOST processes of the invoking user — no
# container engine is involved, and this script says so rather than assuming you
# know. ORCHICON_PLANE_RESIDENCY defaults to `host` here (an explicit setting you
# export is honoured, not overridden).
#
# WHAT THIS DELEGATES — EVERYTHING. There is no lifecycle logic in this script.
# Each verb is dispatched to the CLI's own control surface, so the shell, the
# TUI, the desktop app and this script cannot drift apart:
#   `orchicon instance <verb> [inst]`  when the binary provides it (the shared
#                                      lifecycle surface — preferred, and
#                                      ORCHICON_INSTANCE_CTL overrides it);
#   else the same binary's component verbs: `install` (start), `restart`,
#   `serve --stop`, `serve --status`, `services status|down`. Composing those
#   argv is not a second implementation of the lifecycle — it is the same code
#   the CLI runs, only assembled here.
# The binary is $ORCHICON_BIN, else ./bin/orchicon (built by `make build`), else
# `orchicon` on PATH; if none exists this script stops and says how to build one.
#
# STATUS IS NOT A PORT PROBE. `status` reads the plane's own PID file state
# (`serve --status`) and asks /healthz before it claims health — a bound port
# proves a listener, not a working plane, and an unhealthy plane that still
# holds its port must never read as healthy.
#
# REBUILD IS THE QUICK PATH. `rebuild` compiles and restarts WITHOUT running the
# test suite. That is deliberate: it is the fast inner-loop path. To gate on
# tests/checks, use `make rebuild-<instance>` — the note is printed before it runs.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The binary that owns the lifecycle. Prefer an explicit ORCHICON_BIN, then the
# repo build, then PATH; resolved lazily so `help` works on a checkout that has
# not been built yet.
BIN="${ORCHICON_BIN:-}"
if [ -z "$BIN" ]; then
  if [ -x "$ROOT/bin/orchicon" ]; then
    BIN="$ROOT/bin/orchicon"
  elif command -v orchicon >/dev/null 2>&1; then
    BIN="$(command -v orchicon)"
  fi
fi

# The usage text is a SELF-CONTAINED heredoc, not a slice of this file's own
# header. It used to be a positional `sed` line range over the header, so growing
# the header silently truncated `help` and nothing failed. The delimiter is spelled
# out because a bare `USAGE` terminator would end the block at the `USAGE`
# heading inside it.
usage() {
  cat <<'ORCHICON_SH_USAGE'
orchicon.sh — start/stop the Orchicon plane and its services on the host.

USAGE
  scripts/orchicon.sh start   [dev|prod]   # services + plane for one instance
  scripts/orchicon.sh stop    [dev|prod]   # stop both halves
  scripts/orchicon.sh restart [dev|prod]
  scripts/orchicon.sh status  [dev|prod]   # what is up, and is it really healthy
  scripts/orchicon.sh logs    [dev|prod]   # the plane's log (follows when a tty)
  scripts/orchicon.sh rebuild [dev|prod]   # rebuild + restart (quick path)
  scripts/orchicon.sh help

INSTANCES
  dev   plane http://127.0.0.1:8080   data ~/.local/share/orchicon-dev
  prod  plane http://127.0.0.1:8091   data ~/.local/share/orchicon-prod
  Fully separate data dirs, databases and ports: stopping or restarting one
  never touches the other. Default instance: dev. Anything else is refused.

CLIENT
  TUI   /connect in ~/.orchicon/config, then `orch`
  GUI   open the plane URL above (the plane serves it)

SHAPE
  Services and plane are HOST processes of the invoking user; no container
  engine is involved. ORCHICON_PLANE_RESIDENCY defaults to host here (an
  explicit value you export is honoured).

DELEGATION
  Every verb is dispatched to the CLI's shared control surface
  (`orchicon instance <verb>` when available, else that binary's component
  verbs). This script holds no lifecycle logic, so it cannot drift from the
  CLI, the TUI or the desktop app.

STATUS
  Reads the plane's PID-file state and asks /healthz before calling an instance
  healthy — it never reports health it has not verified.

REBUILD
  Rebuilds and restarts WITHOUT running the test suite (the quick path).
  To gate on tests/checks first, use: make rebuild-<instance>
ORCHICON_SH_USAGE
  exit "${1:-0}"
}

die_inst() {
  echo "orchicon.sh: unknown instance '$1' — expected 'dev' or 'prod'" >&2
  exit 2
}

die_cmd() {
  echo "orchicon.sh: unknown command '$1' — expected start|stop|restart|status|logs|rebuild|help" >&2
  echo >&2
  usage 2
}

die_no_bin() {
  echo "orchicon.sh: cannot find the orchicon binary." >&2
  echo "  looked for: \$ORCHICON_BIN, $ROOT/bin/orchicon, and 'orchicon' on PATH" >&2
  echo "  build one with: make build" >&2
  exit 1
}

# require_bin resolves the binary or stops, loudly and with the fix.
require_bin() {
  [ -n "$BIN" ] && [ -x "$BIN" ] || die_no_bin
}

# instance_paths mirrors the CLI's own per-instance layout, in ONE place, so a
# stop, a status and a log tail can never disagree about which instance they
# mean. Sources, both asserted by scripts/tests/orchicon-cli:
#   plane port      the shared port table (cmd/orchicon/install_ports.go)
#   data dir        hostDataDir  = $HOME/.local/share/orchicon-<instance>
#   plane pid/log   serveStateDir = <data dir>/serve   (cmd/orchicon/serve_state.go)
#   services log    <data dir>/services.log
instance_paths() {
  case "$1" in
    dev) PLANE_PORT=8080 ;;
    prod) PLANE_PORT=8091 ;;
    *) die_inst "$1" ;;
  esac
  DATA_DIR="$HOME/.local/share/orchicon-$1"
  SERVE_STATE_DIR="$DATA_DIR/serve"
  PLANE_LOG="$SERVE_STATE_DIR/logs/orchicon.log"
  SERVICES_LOG="$DATA_DIR/services.log"
  PLANE_URL="http://127.0.0.1:$PLANE_PORT"
}

# plane_health sets HEALTH to healthy | unhealthy | unverified. UNVERIFIED is a
# first-class answer: with no HTTP client available this script does not know,
# and it says so instead of guessing — it never claims health it has not checked.
plane_health() {
  if ! command -v curl >/dev/null 2>&1; then
    HEALTH=unverified
    return 0
  fi
  if curl -fsS --max-time 3 -o /dev/null "$PLANE_URL/healthz" 2>/dev/null; then
    HEALTH=healthy
  else
    HEALTH=unhealthy
  fi
}

# ── shared control surface ───────────────────────────────────────────────────
#
# `host` is THIS script's shape, defaulted rather than forced: an operator who
# exported a different value is not silently overridden by a convenience
# wrapper, and the value is per-invocation so it cannot leak to another instance.
export ORCHICON_PLANE_RESIDENCY="${ORCHICON_PLANE_RESIDENCY:-host}"

# CTL is the shared lifecycle surface (`orchicon instance ...`) when the binary
# provides one, else empty and the component verbs below are used. Probed once.
CTL=""
resolve_ctl() {
  if [ -n "${ORCHICON_INSTANCE_CTL:-}" ]; then
    CTL="$ORCHICON_INSTANCE_CTL"
    return 0
  fi
  require_bin
  if "$BIN" instance --help >/dev/null 2>&1; then
    CTL="$BIN instance"
  fi
}

# ctl_run dispatches one verb to the shared lifecycle surface.
ctl_run() {
  local -a parts=()
  read -r -a parts <<<"$CTL"
  # ${parts[@]+...} rather than "${parts[@]}": this script runs on macOS's bash
  # 3.2 too, where an empty array expansion under `set -u` is an error.
  ${parts[@]+"${parts[@]}"} "$@"
}

# ── component verbs (used when the binary has no `instance` verb) ────────────
#
# These are DIRECT calls to the CLI's own verbs. Nothing here starts, stops,
# probes or supervises anything itself: it chooses the verb, hands it the
# instance, and reports what it observed.
#
# plane_status_out captures `serve --status`; its EXIT CODE is the machine-read
# answer (0 = the PID file names a live process), the text is for the operator.
plane_status_out() {
  ORCHICON_SERVE_STATE_DIR="$SERVE_STATE_DIR" "$BIN" serve --status 2>&1
}

services_status_out() {
  ORCHICON_INSTANCE="$1" "$BIN" services status "$1" 2>&1
}

# start_host: `install` is the CLI's own "bring this instance up on the host"
# verb — services first, then the plane, with the per-instance ports and data
# dir it resolves itself. No engine on this host is a MODE there, not a failure.
start_host() {
  ORCHICON_INSTANCE="$1" "$BIN" install
}

# stop_host stops the PLANE FIRST, then the services: a plane left running over
# a dead database is exactly the state that makes a status check lie. Both calls
# are the CLI's; the verification at the end is ours, because a stop that has not
# been checked is only a claim.
#
# BOTH HALVES ARE VERIFIED, and neither exit code is taken at face value:
#   `serve --stop` exits non-zero BOTH when nothing was running AND when a signal
#   did not land, so the message is chosen by the PID-file state, never by the
#   code — otherwise a plane that is still up gets reported as "nothing was
#   running".
#   `services down` exits non-zero ONLY when the ports did not come free, which is
#   a real failure; swallowing it and printing the success line would be exactly
#   the unverified claim this script exists to prevent.
stop_host() {
  local inst="$1" rc=0 svc_rc=0
  ORCHICON_SERVE_STATE_DIR="$SERVE_STATE_DIR" "$BIN" serve --stop || rc=$?
  if [ "$rc" -ne 0 ] && ! plane_status_out >/dev/null 2>&1; then
    echo "  plane: nothing was running (no live PID file in $SERVE_STATE_DIR)"
  fi
  ORCHICON_INSTANCE="$inst" "$BIN" services down "$inst" || svc_rc=$?
  if plane_status_out >/dev/null 2>&1; then
    echo "✗ orchicon $inst: the plane is STILL running after stop — refusing to report success" >&2
    return 1
  fi
  if [ "$svc_rc" -ne 0 ]; then
    echo "✗ orchicon $inst: the plane stopped but its host services did NOT (a process still holds postgres/nats — see the error above)" >&2
    return 1
  fi
  echo "✓ orchicon $inst stopped (plane and its host services)"
}

status_host() {
  local inst="$1" plane_out="" plane_state="stopped" svc_out="" svc_line="" rc=0

  if plane_out="$(plane_status_out)"; then
    plane_state="running"
  fi
  svc_out="$(services_status_out "$inst")" || rc=$?
  svc_line="$(printf '%s\n' "$svc_out" | head -1)"

  echo "orchicon $inst — plane $PLANE_URL, data $DATA_DIR"
  if [ "$plane_state" = running ]; then
    echo "  plane     $plane_out"
  else
    echo "  plane     not running (no live PID file in $SERVE_STATE_DIR)"
  fi
  echo "  services  $svc_line"
  echo "  logs      $PLANE_LOG"

  if [ "$plane_state" != running ]; then
    echo
    echo "verdict: $inst is DOWN"
    return 1
  fi

  plane_health
  case "$HEALTH" in
    healthy)
      echo "  health    /healthz answered at $PLANE_URL"
      echo
      echo "verdict: $inst is UP and healthy"
      ;;
    unhealthy)
      echo "  health    /healthz did NOT answer at $PLANE_URL"
      echo
      echo "verdict: $inst is UP but NOT healthy"
      return 1
      ;;
    *)
      echo "  health    UNVERIFIED — no curl on PATH, so /healthz was not asked"
      echo
      echo "verdict: $inst is UP (health UNVERIFIED — install curl to verify it)"
      ;;
  esac
  [ "$rc" -eq 0 ] || echo "  note      the services report above says they are not all up"
  return 0
}

logs_host() {
  local inst="$1"
  if [ ! -f "$PLANE_LOG" ]; then
    echo "orchicon.sh: no plane log for $inst at $PLANE_LOG" >&2
    echo "  it is written when the plane runs; start it with: scripts/orchicon.sh start $inst" >&2
    return 1
  fi
  echo "── $PLANE_LOG (services log: $SERVICES_LOG) ──" >&2
  if [ -t 1 ]; then
    tail -n 100 -f "$PLANE_LOG"
  else
    tail -n 100 "$PLANE_LOG"
  fi
}

# rebuild_host is the QUICK path: compile then restart, with no test suite. The
# gated path is `make rebuild-<instance>` (build → ci → migrate-hash → restart).
rebuild_host() {
  local inst="$1"
  command -v make >/dev/null 2>&1 || {
    echo "orchicon.sh: no 'make' on PATH — cannot rebuild. Build with your toolchain, then: scripts/orchicon.sh restart $inst" >&2
    return 1
  }
  make -C "$ROOT" build
  stop_host "$inst"
  start_host "$inst"
}

# ── per-verb wiring ─────────────────────────────────────────────────────────
#
# One code path per verb, per instance. Everything below decides WHICH shared
# verb to call and WHAT IT DID; nothing here implements a lifecycle.

start_instance() {
  local inst="$1"
  instance_paths "$inst"
  if [ -n "$CTL" ]; then
    ctl_run start "$inst"
  else
    start_host "$inst"
  fi
  print_summary "$inst"
}

# print_summary says plainly what just happened and where things are. It is the
# reason this script exists: after `start` the developer must not have to guess
# the plane URL, the client, or where the log went.
print_summary() {
  local inst="$1"
  instance_paths "$inst"
  echo
  echo "── orchicon $inst is up ──────────────────────────────────────────"
  echo "  plane   $PLANE_URL"
  echo "  gui     open $PLANE_URL in a browser"
  echo "  tui     orch   (profile in ~/.orchicon/config)"
  echo "  logs    scripts/orchicon.sh logs $inst"
  echo "  shape   host — services and plane are your user's processes, no container engine"
  echo "  data    $DATA_DIR"
}

stop_instance() {
  local inst="$1"
  instance_paths "$inst"
  if [ -n "$CTL" ]; then
    ctl_run stop "$inst"
    echo "✓ orchicon $inst stopped (plane and its host services)"
  else
    stop_host "$inst"
  fi
}

restart_instance() {
  local inst="$1"
  instance_paths "$inst"
  if [ -n "$CTL" ]; then
    ctl_run restart "$inst"
  else
    # down-then-up rather than the CLI's `restart` verb: this must work on a
    # fresh checkout where nothing is installed yet, and `restart` correctly
    # refuses an instance with no install record. Refusing is right for that
    # verb and wrong here, where "restart" must also mean "start".
    stop_host "$inst"
    start_host "$inst"
  fi
  echo "✓ orchicon $inst restarted"
}

status_instance() {
  local inst="$1"
  instance_paths "$inst"
  if [ -n "$CTL" ]; then
    ctl_run status "$inst"
  else
    status_host "$inst"
  fi
}

logs_instance() {
  local inst="$1"
  instance_paths "$inst"
  if [ -n "$CTL" ]; then
    ctl_run logs "$inst"
  else
    logs_host "$inst"
  fi
}

rebuild_instance() {
  local inst="$1"
  instance_paths "$inst"
  # Printed BEFORE anything runs: the operator must know what they asked for
  # while it is still running, not after the fact.
  echo "note: this rebuilds and restarts WITHOUT running the test suite."
  echo "      to gate on tests/checks, use: make rebuild-$inst"
  if [ -n "$CTL" ]; then
    ctl_run rebuild "$inst"
  else
    rebuild_host "$inst"
  fi
}

cmd="${1:-help}"
inst="${2:-dev}"

case "$cmd" in
  start|up|stop|down|restart|status|logs|rebuild)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    resolve_ctl
    case "$cmd" in
      start|up) start_instance "$inst" ;;
      stop|down) stop_instance "$inst" ;;
      restart) restart_instance "$inst" ;;
      status) status_instance "$inst" ;;
      logs) logs_instance "$inst" ;;
      rebuild) rebuild_instance "$inst" ;;
    esac
    ;;
  help|-h|--help|"")
    usage 0
    ;;
  *)
    die_cmd "$cmd"
    ;;
esac
