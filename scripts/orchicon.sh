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
#   scripts/orchicon.sh start   [dev|prod|test] # services container + host plane
#   scripts/orchicon.sh stop    [dev|prod|test] # stop both halves
#   scripts/orchicon.sh restart [dev|prod|test]
#   scripts/orchicon.sh status  [dev|prod|test] # what is up, and is it healthy
#   scripts/orchicon.sh logs    [dev|prod|test] # the plane's log
#   scripts/orchicon.sh rebuild [dev|prod|test] # rebuild image + restart (see note below)
#   scripts/orchicon.sh uninstall [dev|prod|test] --yes
#                                              # remove ONE instance AND its data
#                                              # (destructive; needs --yes)
#   scripts/orchicon.sh help
#
# INSTANCES. `dev` listens on 127.0.0.1:8080 with its services container on
# 5432; `prod` on 127.0.0.1:8091 with its container on 5433; `test` on
# 127.0.0.1:8092 with its container on 5434 (a throwaway instance for
# installer/upgrade trials — its database is its own `fresh` volume, never
# dev/prod's legacy compose volume). They are fully separate — separate
# databases, separate data dirs, separate permission policies — so stopping (or
# UNINSTALLING) one never touches the others. Default: dev.
#
# THE INSTANCE TABLE ITSELF IS NOT HERE: it lives in scripts/lib/instances.sh,
# the one place the shell side enumerates dev/test/prod, and the Go installer's
# instanceOrder is asserted against it by
# TestInstallPortDefaultsMatchShellInstanceTable.
#
# CLIENT
#   The TUI talks to an instance through a profile in ~/.orchicon/config:
#     /connect          pick or add a profile (url + token), then
#     orch              start the TUI
#   The GUI is served BY the plane: open http://127.0.0.1:8080 (dev) or
#   http://127.0.0.1:8091 (prod).
#
# UNINSTALL IS DEFINED HERE, NOT DELEGATED. Removing an instance and its data is
# instance-scoped, destructive and confirmed: `scripts/orchicon.sh uninstall
# <dev|prod|test> --yes` (or ORCHICON_CONFIRM_DESTROY=1) prints exactly what it
# will remove, refuses without confirmation, never touches a sibling instance,
# and never removes dev/prod's legacy PG_VOLUME. The body lives in
# scripts/lib/teardown.sh; container.sh's legacy `uninstall` verb sources the
# same file, so there is one teardown, not two.
#
# WHAT THIS DELEGATES. The launch logic — the services container,
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

# PROJECT_ROOT is what lib/teardown.sh's host_plane_bin resolves bin/orchicon
# from; set before sourcing so the lib uses this script's root.
PROJECT_ROOT="$ROOT"
. "$ROOT/scripts/lib/instances.sh"
. "$ROOT/scripts/lib/engine.sh"
. "$ROOT/scripts/lib/teardown.sh"

if [ ! -x "$CONTAINER_SH" ]; then
  echo "orchicon.sh: cannot find scripts/container.sh (looked in $ROOT/scripts)" >&2
  exit 1
fi

# Usage is the header block itself, printed up to the first non-comment line:
# a hardcoded line RANGE silently truncated this the moment the header grew.
usage() {
  awk 'NR>2 { if ($0 !~ /^#/) exit; sub(/^# ?/,""); print }' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

die_inst() {
  echo "orchicon.sh: unknown instance '$1' — expected one of: $ORCHICON_INSTANCES" >&2
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
    case "$inst" in dev|prod|test) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" up
    echo
    echo "── orchicon $inst is up ──────────────────────────────────────────"
    instance_info "$inst"
    echo "  plane   http://127.0.0.1:$PLANE_HTTP_PORT"
    echo "  gui     open the URL above in a browser"
    echo "  tui     orch   (profile in ~/.orchicon/config)"
    echo "  logs    scripts/orchicon.sh logs $inst"
    ;;
  stop|down)
    case "$inst" in dev|prod|test) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" down
    echo "✓ orchicon $inst stopped (plane and its services container)"
    ;;
  restart)
    case "$inst" in dev|prod|test) ;; *) die_inst "$inst" ;; esac
    host_run "$inst" down
    host_run "$inst" up
    echo "✓ orchicon $inst restarted"
    ;;
  status)
    case "$inst" in dev|prod|test|"") ;; *) die_inst "$inst" ;; esac
    "$CONTAINER_SH" status "$inst"
    ;;
  logs)
    case "$inst" in dev|prod|test) ;; *) die_inst "$inst" ;; esac
    "$CONTAINER_SH" logs "$inst"
    ;;
  rebuild)
    case "$inst" in dev|prod|test) ;; *) die_inst "$inst" ;; esac
    # container.sh rebuild does NOT run the test suite; the Makefile target does
    # (build → ci → migrate-hash → image → restart). For a change you intend to
    # ship, use `make rebuild-$inst`. This verb is the quick path: rebuild the
    # image and restart, no checks.
    echo "note: this rebuilds and restarts WITHOUT running the test suite."
    echo "      to gate on tests/checks, use: make rebuild-$inst"
    host_run "$inst" rebuild
    ;;
  uninstall)
    # A DESTRUCTIVE VERB MUST NOT DEFAULT. container.sh's legacy arm defaults to
    # `dev`; here an omitted instance is refused outright — this is the verb the
    # docs point at, so "remove the wrong instance because a word was missing" is
    # not an acceptable default.
    inst="${2:-}"
    if [ -z "$inst" ]; then
      echo "orchicon.sh: uninstall needs an explicit instance ($ORCHICON_INSTANCES)" >&2
      exit 2
    fi
    if ! instance_exists "$inst"; then
      die_inst "$inst"
    fi
    shift 2 || true
    # --yes is the in-script confirmation; ORCHICON_CONFIRM_DESTROY=1 is the
    # env-only form. Anything else is refused rather than ignored.
    for arg in "$@"; do
      case "$arg" in
        --yes|-y) CONFIRM_DESTROY=1 ;;
        *) echo "orchicon.sh: unexpected argument '$arg' (uninstall takes <dev|prod|test> [--yes])" >&2; exit 2 ;;
      esac
    done
    uninstall_instance "$inst"
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
