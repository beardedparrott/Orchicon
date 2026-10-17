#!/usr/bin/env bash
#
# orchicon.sh — START/STOP AN ORCHICON INSTANCE ON THE HOST.
#
# WHY THIS EXISTS. An instance's PLANE runs on the HOST (a host user is the user);
# the SERVICES it needs (Postgres, NATS, telemetry) are started and stopped
# alongside it. There is no image and no instance container: this is the entry
# point to reach for day to day, and it starts BOTH halves, stops BOTH halves and
# tells you what is running.
#
# USAGE
#   scripts/orchicon.sh start   [dev|prod|test]   # services + host plane
#   scripts/orchicon.sh stop    [dev|prod|test]   # stop both halves
#   scripts/orchicon.sh restart [dev|prod|test]
#   scripts/orchicon.sh status  [dev|prod|test]   # what is up, and is it healthy;
#                                                 # NO argument reports EVERY instance
#   scripts/orchicon.sh logs    [dev|prod|test]   # the plane's log
#   scripts/orchicon.sh rebuild [dev|prod|test]   # restart only — see the note below
#   scripts/orchicon.sh help
#
# INSTANCES. `dev` listens on 127.0.0.1:8080 with its services on 5432; `prod` on
# 127.0.0.1:8091 with its services on 5433; `test` is a throwaway on its own
# offset ports. They are fully separate — separate databases, separate data dirs,
# separate permission policies — so stopping one never touches another.
# Default: dev. `status` is the exception: with no argument it reports them all.
#
# INSTANCE NAMES ARE VALIDATED, NOT ALLOWLISTED. The name becomes a state
# directory ($HOME/.local/share/orchicon-<inst>), so only what cannot be a path is
# refused: `/`, `\` and `..`. Anything else is the port table's business to reject
# with a real message — the old hardcoded dev|prod allowlist is what made
# `make rebuild-test` fail with "unknown instance 'test'".
#
# CLIENT
#   The TUI talks to an instance through a profile in ~/.orchicon/config:
#     /connect          pick or add a profile (url + token), then
#     orch              start the TUI
#   The GUI is served BY the plane: open http://127.0.0.1:8080 (dev) or
#   http://127.0.0.1:8091 (prod).
#
# THE VERBS ARE THE CONTRACT. start|stop|restart|status|logs|rebuild are what other
# tooling calls — the Makefile's instance-* targets name these and nothing further,
# so the implementation behind them can change without an edit up there. The launch
# logic (the services, their ports, the plane's loopback + bridge listeners, the
# per-instance data dirs, the KEK migration) is NOT duplicated here: this script's
# job is to make the host shape the obvious default and to say plainly what it did.
#
# RESIDENCY. Host residency is pinned explicitly (ORCHICON_PLANE_RESIDENCY=host)
# rather than inherited, so this script cannot silently start a shape other than the
# one the product ships.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINER_SH="$ROOT/scripts/container.sh"

if [ ! -x "$CONTAINER_SH" ]; then
  echo "orchicon.sh: cannot find scripts/container.sh (looked in $ROOT/scripts)" >&2
  exit 1
fi

usage() {
  # THE HEADER IS THE USAGE TEXT: print the leading comment block (line 3 up to the
  # first non-comment line) so the two cannot drift apart.
  awk 'NR < 3 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

# validate_instance_name refuses only a name that cannot BE a state directory. The
# same rule cmd/orchicon/restart.go applies, for the same reason: the name is a
# path, so a separator or a parent reference escapes it.
validate_instance_name() {
  case "$1" in
    */*|*\\*|*..*)
      echo "orchicon.sh: invalid instance name '$1' — it becomes a state directory, so '/', '\\' and '..' are refused" >&2
      exit 2
      ;;
  esac
}

# host_up/host_down pin the residency for the ONE invocation, per instance.
# container.sh reads it per call, so nothing leaks between instances.
host_run() {
  local inst="$1"; shift
  ORCHICON_PLANE_RESIDENCY=host "$CONTAINER_SH" "$@" "$inst"
}

cmd="${1:-help}"
# NO DEFAULT HERE. `status` must be able to mean "every instance", so the default
# is applied by each verb that needs one instance (below), not once at the top.
inst="${2:-}"

case "$cmd" in
  start|up)
    inst="${inst:-dev}"; validate_instance_name "$inst"
    host_run "$inst" up
    case "$inst" in
      dev)  plane_url="http://127.0.0.1:8080" ;;
      prod) plane_url="http://127.0.0.1:8091" ;;
      *)    plane_url="(this instance's own port — see: scripts/orchicon.sh status $inst)" ;;
    esac
    echo
    echo "── orchicon $inst is up ──────────────────────────────────────────"
    echo "  plane   $plane_url"
    echo "  gui     open the URL above in a browser"
    echo "  tui     orch   (profile in ~/.orchicon/config)"
    echo "  logs    scripts/orchicon.sh logs $inst"
    ;;
  stop|down)
    inst="${inst:-dev}"; validate_instance_name "$inst"
    host_run "$inst" down
    echo "✓ orchicon $inst stopped (plane and its services)"
    ;;
  restart)
    inst="${inst:-dev}"; validate_instance_name "$inst"
    host_run "$inst" down
    host_run "$inst" up
    echo "✓ orchicon $inst restarted"
    ;;
  status)
    # WITH NO INSTANCE: report them ALL. "What is running on this host?" is the
    # question an operator asks without naming one, and it is what `make
    # instance-ps` calls — a `dev` default here made that target unable to answer.
    if [ -n "$inst" ]; then validate_instance_name "$inst"; fi
    "$CONTAINER_SH" status "$inst"
    ;;
  logs)
    inst="${inst:-dev}"; validate_instance_name "$inst"
    "$CONTAINER_SH" logs "$inst"
    ;;
  rebuild)
    inst="${inst:-dev}"; validate_instance_name "$inst"
    # This verb does NOT run the test suite; the Makefile target does (build → ci →
    # migrate-hash → restart). For a change you intend to ship, use
    # `make rebuild-$inst`. This is the quick path: rebuild and restart, no checks.
    echo "note: this rebuilds and restarts WITHOUT running the test suite."
    echo "      to gate on tests/checks, use: make rebuild-$inst"
    host_run "$inst" rebuild
    ;;
  help|-h|--help|"")
    usage 0
    ;;
  *)
    echo "orchicon.sh: unknown command '$cmd'" >&2
    echo >&2
    usage 2
    ;;
esac
