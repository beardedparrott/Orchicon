#!/usr/bin/env bash
#
# orchicon.sh — START/STOP THE ORCHICON PLANE ON THE HOST.
#
# WHY THIS EXISTS. Orchicon's plane runs on the HOST (a host user is the user);
# the SERVICES it needs (Postgres, NATS, telemetry) still run in the instance's
# container. `scripts/container.sh` owns all of that, but its name says
# "container" and its default verb set does not make the two halves obvious. This
# is the entry point to reach for day to day: it starts BOTH halves, stops BOTH
# halves, and tells you what is running.
#
# USAGE
#   scripts/orchicon.sh start   [dev|prod]     # services container + host plane
#   scripts/orchicon.sh stop    [dev|prod]     # stop both halves
#   scripts/orchicon.sh restart [dev|prod]
#   scripts/orchicon.sh status  [dev|prod]     # what is up, and is it healthy
#   scripts/orchicon.sh logs    [dev|prod]     # the plane's log
#   scripts/orchicon.sh rebuild [dev|prod]     # rebuild image + restart (see note below)
#   scripts/orchicon.sh help
#
# INSTANCES. `dev` listens on 127.0.0.1:8080 with its services container on
# 5432; `prod` on 127.0.0.1:8091 with its container on 5433. They are fully
# separate — separate databases, separate data dirs, separate permission
# policies — so stopping one never touches the other. Default: dev.
#
# CLIENT
#   The TUI talks to an instance through a profile in ~/.orchicon/config:
#     /connect          pick or add a profile (url + token), then
#     orch              start the TUI
#   The GUI is served BY the plane: open http://127.0.0.1:8080 (dev) or
#   http://127.0.0.1:8091 (prod).
#
# WHAT THIS DELEGATES. Everything. The launch logic — the services container,
# its published ports, the host plane's loopback + docker-bridge listeners, the
# per-instance data dirs and the KEK migration — lives in scripts/container.sh
# and is not duplicated here. This script's only job is to make the HOST shape
# the obvious default and to say plainly what it did.
#
# RESIDENCY. Host residency is pinned explicitly (ORCHICON_PLANE_RESIDENCY=host)
# rather than inherited, so this script cannot silently start a container-plane
# instance and leave you wondering why the services container has no plane in it.
# To go back to the all-in-container shape, use container.sh directly:
#   scripts/container.sh up dev container
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINER_SH="$ROOT/scripts/container.sh"

if [ ! -x "$CONTAINER_SH" ]; then
  echo "orchicon.sh: cannot find scripts/container.sh (looked in $ROOT/scripts)" >&2
  exit 1
fi

usage() {
  sed -n '3,45p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

die_inst() {
  echo "orchicon.sh: unknown instance '$1' — expected 'dev' or 'prod'" >&2
  exit 2
}

# host_up/host_down pin the residency for the ONE invocation, per instance.
# container.sh reads it per call, so nothing leaks between instances.
host_run() {
  local inst="$1"; shift
  ORCHICON_PLANE_RESIDENCY=host "$CONTAINER_SH" "$@" "$inst"
}

cmd="${1:-help}"
inst="${2:-dev}"

case "$cmd" in
  start|up)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" up
    echo
    echo "── orchicon $inst is up ──────────────────────────────────────────"
    echo "  plane   http://127.0.0.1:$([ "$inst" = prod ] && echo 8091 || echo 8080)"
    echo "  gui     open the URL above in a browser"
    echo "  tui     orch   (profile in ~/.orchicon/config)"
    echo "  logs    scripts/orchicon.sh logs $inst"
    ;;
  stop|down)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" down
    echo "✓ orchicon $inst stopped (plane and its services container)"
    ;;
  restart)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" down
    host_run "$inst" up
    echo "✓ orchicon $inst restarted"
    ;;
  status)
    case "$inst" in dev|prod|"") ;; *) die_inst "$inst" ;; esac
    "$CONTAINER_SH" status "$inst"
    ;;
  logs)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    "$CONTAINER_SH" logs "$inst"
    ;;
  rebuild)
    case "$inst" in dev|prod) ;; *) die_inst "$inst" ;; esac
    # container.sh rebuild does NOT run the test suite; the Makefile target does
    # (build → ci → migrate-hash → image → restart). For a change you intend to
    # ship, use `make rebuild-$inst`. This verb is the quick path: rebuild the
    # image and restart, no checks.
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
