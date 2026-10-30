#!/usr/bin/env bash
#
# runtime.sh — BUILD THE STOCK WORKFLOW RUNTIME IMAGES, AND RUN THE RUNTIME DAEMON.
#
# WHY THIS EXISTS. Both of these used to be verbs of scripts/container.sh, which
# is being retired with the container-resident instance. Neither has anything to
# do with an instance's lifecycle: the images are a pure function of the
# Dockerfiles, and the daemon is a host process that serves a unix socket. They
# only ever needed a container ENGINE (podman), never an instance. So they live
# here, and the engine is the only thing this script requires.
#
#   scripts/runtime.sh build           # the three stock images, version-gated
#   scripts/runtime.sh daemon-start    # start (or refresh) the host daemon
#   scripts/runtime.sh daemon-stop     # stop it
#   scripts/runtime.sh daemon-status   # is it answering, and on which socket
#   scripts/runtime.sh help
#
# ENGINE. `${ORCHICON_ENGINE:-podman}`. Every engine call goes through it, so a
# docker host works with ORCHICON_ENGINE=docker and nothing else changes.
#
# THE IMAGES ARE VERSION-GATED, NOT UNCONDITIONALLY REBUILT. Each stock image is
# labelled org.orchicon.runtime.version = "<app-version>-<sha12 of its Dockerfile>"
# (the :gui / :dev variants embed the BASE's version, so a base change cascades to
# them). An image whose label already matches is skipped, because the orchicon
# binary is NOT baked into these images — the daemon bind-mounts its own executable
# into every runtime container — so a normal rebuild (binary changed, Dockerfiles
# unchanged) has nothing to build. Touch a Dockerfile (or bump the version) and the
# affected variants rebuild. FORCE_RUNTIME=1 / ORCHICON_FORCE_RUNTIME_REBUILD=1
# rebuilds regardless.
#
# AN IMAGE CARRYING org.orchicon.runtime.spec-version IS NEVER TOUCHED. That label
# means the runtime daemon built it from a canned stock row the operator edited and
# redeployed: it is tenant-owned, and a pristine rebuild here would silently
# clobber it.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGINE="${ORCHICON_ENGINE:-podman}"
RUNTIME_CONTEXT="$ROOT/deploy/runtime"
SOCKET_DIR="${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}"
SOCKET="${ORCHICON_RUNTIME_SOCKET:-$SOCKET_DIR/runtime.sock}"
DAEMON_LOG="${ORCHICON_RUNTIME_DAEMON_LOG:-/tmp/orchicon-runtime-daemon.log}"
# THE VARIANT TAG NAMES ARE THE CATALOGUE'S, and they are ONE set across the tree.
#
# These used to be "orchicon-runtime:local-gui" and "orchicon-runtime:orchicon-dev" while the daemon's
# allowlist emitted ":gui-<tag>"/":dev-<tag>" and the catalogue's local tags are ":gui"/":dev" — three
# spellings for two images. The consequence was not cosmetic: a developer who ran this script and a
# developer who fetched from the Runtime Images page got DIFFERENT images for the same variant, and the
# daemon's allowlist matched neither. The variant is the identity; the tag is how it is addressed, and
# there is one spelling of it now:
#
#   base  orchicon-runtime:local
#   gui   orchicon-runtime:gui
#   dev   orchicon-runtime:dev
#
# The base keeps ":local" deliberately: it is the LOCALLY-BUILT base on a dev host, and the daemon's
# ORCHICON_RUNTIME_IMAGE points at it. The catalogue's published base is the ghcr ref, and the two are
# different artifacts by design.
BASE_IMAGE="${ORCHICON_RUNTIME_IMAGE:-orchicon-runtime:local}"
GUI_IMAGE="${ORCHICON_RUNTIME_GUI_IMAGE:-orchicon-runtime:gui}"
DEV_IMAGE="${ORCHICON_RUNTIME_DEV_IMAGE:-orchicon-runtime:dev}"

log()     { printf '==> %s\n' "$*"; }
log_dim() { printf '    %s\n' "$*"; }
die()     { printf 'runtime.sh: %s\n' "$*" >&2; exit 1; }

usage() {
  # The header IS the usage text: print its leading comment block (line 3 to the
  # first non-comment line) so the two cannot drift apart.
  awk 'NR < 3 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

require_engine() {
  command -v "$ENGINE" >/dev/null 2>&1 || die \
    "container engine '$ENGINE' not found on PATH — install podman, or set ORCHICON_ENGINE to the engine binary you have"
  "$ENGINE" info >/dev/null 2>&1 || die "'$ENGINE info' failed — the engine is not usable by this user (rootless setup? socket?)"
}

# sha12 prints the first 12 hex chars of a file's SHA-256 — the "did the build
# inputs change?" signal behind the version gate (same git-tag + content-hash
# pattern as the rest of Orchicon).
sha12() { sha256sum "$1" | cut -c1-12; }

# app_version resolves the version half of the label. --match 'v[0-9]*' keeps a
# MOVING prerelease tag (canary) out of it: git describe accepts any tag and
# tie-breaks lexically, so `canary` once beat `v0.5.45` and a rebuild of a real
# release reported "canary". Same rule as the Makefile's VERSION.
app_version() {
  git -C "$ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo dev
}

# image_version computes the desired label for one variant: the Dockerfile's hash,
# under the version its base carries ("" means "this IS the base").
image_version() { # <dockerfile> <version-prefix>
  local dockerfile="$1" prefix="$2"
  [ -f "$dockerfile" ] || { printf ''; return; }
  printf '%s-%s' "$prefix" "$(sha12 "$dockerfile")"
}

image_label() { # <tag> <label-key>
  "$ENGINE" image inspect "$1" --format "{{index .Config.Labels \"$2\"}}" 2>/dev/null || true
}

# image_needs_rebuild reports whether one variant must be (re)built: the image is
# missing, its version label differs from the desired one, or a force variable is
# set. A tenant-owned image (spec-version label) never rebuilds.
image_needs_rebuild() { # <tag> <desired-version>
  local tag="$1" want="$2"
  if [ "${FORCE_RUNTIME:-}" = "1" ] || [ "${ORCHICON_FORCE_RUNTIME_REBUILD:-}" = "1" ]; then
    return 0
  fi
  [ -n "$(image_label "$tag" org.orchicon.runtime.spec-version)" ] && return 1
  [ "$(image_label "$tag" org.orchicon.runtime.version)" != "$want" ]
}

build_variant() { # <dockerfile> <tag> <desired-version> [base-tag]
  local dockerfile="$1" tag="$2" want="$3" base="${4:-}"
  if ! image_needs_rebuild "$tag" "$want"; then
    log_dim "image $tag up to date (runtime v$want) — skipping"
    return 0
  fi
  local -a args=(build --label "org.orchicon.runtime.version=$want")
  [ -n "$base" ] && args+=(--build-arg "BASE_IMAGE=$base")
  log "building $tag from ${dockerfile#"$ROOT"/} (runtime v$want${base:+, base $base})"
  "$ENGINE" "${args[@]}" -f "$dockerfile" -t "$tag" "$RUNTIME_CONTEXT"
  log_dim "image $tag built"
}

cmd_build() {
  require_engine
  local app base_version
  app="$(app_version)"
  base_version="$(image_version "$RUNTIME_CONTEXT/Dockerfile" "$app")"
  [ -n "$base_version" ] || die "missing $RUNTIME_CONTEXT/Dockerfile — nothing to build from"

  build_variant "$RUNTIME_CONTEXT/Dockerfile" "$BASE_IMAGE" "$base_version"
  # The two variants are OPTIONAL: a tree that ships only the base Dockerfile must
  # still build, and an absent file is not an empty version to compare against (the
  # gate would then report "needs rebuild" and hand the engine `-f ''`). Same guard
  # the ported code in container.sh had.
  if [ -f "$RUNTIME_CONTEXT/Dockerfile.gui" ]; then
    build_variant "$RUNTIME_CONTEXT/Dockerfile.gui" "$GUI_IMAGE" \
      "$(image_version "$RUNTIME_CONTEXT/Dockerfile.gui" "$base_version")" "$BASE_IMAGE"
  else
    log_dim "no deploy/runtime/Dockerfile.gui — skipping the :gui variant"
  fi
  if [ -f "$RUNTIME_CONTEXT/Dockerfile.dev" ]; then
    build_variant "$RUNTIME_CONTEXT/Dockerfile.dev" "$DEV_IMAGE" \
      "$(image_version "$RUNTIME_CONTEXT/Dockerfile.dev" "$base_version")" "$BASE_IMAGE"
  else
    log_dim "no deploy/runtime/Dockerfile.dev — skipping the :dev variant"
  fi

  # A rebuild repoints the tag and leaves the previous image dangling; repeated dev
  # builds otherwise accumulate tens of GB of unreferenced layers. Dangling only —
  # a running container pins its image by ID, so nothing in use is touched.
  log_dim "pruning dangling images from this build"
  "$ENGINE" image prune -f --filter "dangling=true" >/dev/null 2>&1 || true
  log "runtime images are up to date ($BASE_IMAGE, $GUI_IMAGE, $DEV_IMAGE)"
}

daemon_healthy() {
  [ -S "$SOCKET" ] && curl -s --unix-socket "$SOCKET" http://runtime/v1/health >/dev/null 2>&1
}

# daemon_is_stale reports whether the RUNNING daemon serves an older binary than
# bin/orchicon. The daemon copies its own executable to $SOCKET_DIR/orchicon and
# bind-mounts THAT copy into every runtime container, so a stale copy means every
# container it creates runs the pre-rebuild toolset. A missing stable copy is stale
# by definition (the next start re-creates it); a missing bin/orchicon is not (the
# daemon keeps running its own inode and its copy stays valid).
daemon_is_stale() {
  local stable="$SOCKET_DIR/orchicon" fresh="$ROOT/bin/orchicon"
  [ -f "$fresh" ] || return 1
  [ -f "$stable" ] || return 0
  ! cmp -s "$fresh" "$stable"
}

# daemon_pid prints the pid of the HOST daemon. pgrep is preferred, but slim images
# ship without procps, and its absence failed in the worst possible direction:
# `pgrep: command not found` on stderr and an EMPTY pid, so `make runtime-stop`
# reported a clean no-op ("no runtime daemon is running") while the daemon that
# `make runtime-daemon` had just started kept serving the socket — a stop that
# silently cannot stop what this script starts. The /proc fallback matches the
# daemon's OWN argv (basename "orchicon", then exactly "runtime-daemon") rather
# than its command TEXT, so a shell whose command line merely mentions the daemon
# never matches itself.
# pid_is_daemon reports whether pid is REALLY the runtime daemon: argv[0] is the orchicon binary and
# argv[1] is exactly "runtime-daemon".
#
# THE MATCH IS ON ARGV, NOT ON COMMAND TEXT, and that is a correctness requirement rather than
# strictness. `pgrep -f 'orchicon runtime-daemon'` matches any process whose command line merely
# CONTAINS that string — including a shell running `make runtime-stop`, an editor with the file open,
# or a `bash -c` that happens to mention it. Measured while adding daemon-refresh: four pids matched,
# ONE was the daemon, and the other three were the invoking shell. A stop built on that kills whatever
# it finds first, which is how `make runtime-stop` could kill an unrelated process and then report the
# daemon "still running" because it had killed the wrong one.
pid_is_daemon() {
  local pid="$1" cmdfile="/proc/$1/cmdline"
  if [ -r "$cmdfile" ]; then
    local first second
    first="$(tr '\0' '\n' <"$cmdfile" 2>/dev/null | sed -n '1p' || true)"
    case "${first##*/}" in orchicon) ;; *) return 1 ;; esac
    second="$(tr '\0' '\n' <"$cmdfile" 2>/dev/null | sed -n '2p' || true)"
    [ "$second" = "runtime-daemon" ]
    return $?
  fi
  # No /proc (macOS): verify through ps. `args=` is the argv joined by spaces, so the first two tokens
  # are the ones this rule is about.
  local args
  args="$(ps -o args= -p "$pid" 2>/dev/null || true)"
  [ -n "$args" ] || return 1
  set -- $args
  [ "$#" -ge 2 ] || return 1
  case "${1##*/}" in orchicon) ;; *) return 1 ;; esac
  [ "$2" = "runtime-daemon" ]
}

# daemon_pids prints EVERY live runtime daemon. Plural on purpose: more than one can exist, and when
# two do, the SECOND one steals the socket — ListenAndServe removes the path before binding it — so
# the first becomes an orphan that still holds containers and leases while serving nothing. A stop
# that handled one pid would leave exactly that orphan behind.
#
# Zombies are excluded: a killed daemon whose parent has not reaped it still appears in /proc, and
# counting it makes a successful stop look like a failed one.
daemon_pids() {
  local d pid
  if [ -d /proc ]; then
    for d in /proc/[0-9]*; do
      pid="${d#/proc/}"
      [ "$pid" = "$$" ] && continue
      # A pid can vanish between the glob and the read; silence that race rather than printing
      # "/proc/NNN/cmdline: No such file or directory" into a rebuild's output.
      [ -r "$d/stat" ] || continue
      [ "$(awk '{print $3}' "$d/stat" 2>/dev/null)" = "Z" ] && continue
      pid_is_daemon "$pid" && printf '%s\n' "$pid"
    done
    return 0
  fi
  for pid in $(pgrep -f 'orchicon runtime-daemon' 2>/dev/null || true); do
    [ "$pid" = "$$" ] && continue
    pid_is_daemon "$pid" && printf '%s\n' "$pid"
  done
  return 0
}

# daemon_pid prints the first live daemon (empty when none). Kept for callers that need one.
#
# THE LIST IS CONSUMED BEFORE IT IS SLICED. `daemon_pids | head -1` lets head close the pipe while the
# producer is still writing, so the producer dies of SIGPIPE — and under this script's
# `set -o pipefail` that becomes the FUNCTION's exit status, killing `make runtime-stop` with 141 while
# the daemon was never touched. Measured exactly that way.
daemon_pid() {
  local all
  all="$(daemon_pids || true)"
  printf '%s\n' "$all" | sed -n '1p'
}

cmd_daemon_start() {
  if daemon_healthy; then
    if daemon_is_stale; then
      log "runtime daemon is serving a STALE binary (its bind-mount copy predates bin/orchicon) — restarting it"
      log_dim "every existing runtime container is reaped and re-warmed from the fresh binary on the next run"
      local pid
      pid="$(daemon_pid)"
      if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
        local _i
        for _i in $(seq 1 20); do
          daemon_healthy || break
          sleep 0.25
        done
      fi
    else
      log_dim "runtime daemon already up and fresh ($SOCKET)"
      return 0
    fi
  fi

  [ -x "$ROOT/bin/orchicon" ] || die "bin/orchicon not found — run 'make instance-build' (or make build) first"

  # Register the locally-built :gui / :dev variants in the daemon's image allowlist
  # so they appear in the work-item dropdown. An operator ORCHICON_RUNTIME_IMAGES
  # overrides the list, and the base image is always allowlisted.
  local images="" img
  for img in "$GUI_IMAGE" "$DEV_IMAGE"; do
    if "$ENGINE" image inspect "$img" >/dev/null 2>&1; then
      images="${images:+$images,}$img"
    fi
  done

  log "starting runtime daemon (socket $SOCKET, log $DAEMON_LOG)"
  ORCHICON_RUNTIME_IMAGE="${ORCHICON_RUNTIME_IMAGE:-$BASE_IMAGE}" \
  ORCHICON_RUNTIME_IMAGES="${ORCHICON_RUNTIME_IMAGES:-$images}" \
  setsid nohup "$ROOT/bin/orchicon" runtime-daemon </dev/null >"$DAEMON_LOG" 2>&1 &

  local _i
  for _i in $(seq 1 20); do
    if daemon_healthy; then
      log "runtime daemon up ($SOCKET)"
      return 0
    fi
    sleep 0.25
  done
  die "runtime daemon failed to start — see $DAEMON_LOG"
}

cmd_daemon_stop() {
  local pids
  pids="$(daemon_pids || true)"
  if [ -z "$pids" ]; then
    log_dim "no runtime daemon is running"
    return 0
  fi
  # EVERY daemon, not just the first. Two can be alive at once because ListenAndServe removes the
  # socket path before binding it, so a second daemon takes the socket over and the first becomes an
  # orphan that still holds containers and leases while serving nothing. Stopping one and declaring
  # success is what leaves that orphan behind — and it is also what made a stop report the daemon
  # "still running after 10s" when it had merely killed a different pid.
  local pid
  for pid in $pids; do
    log "stopping runtime daemon (pid $pid)"
    kill "$pid" 2>/dev/null || true
  done
  local _i remaining
  for _i in $(seq 1 40); do
    remaining="$(daemon_pids || true)"
    [ -z "$remaining" ] && { log "runtime daemon stopped"; return 0; }
    sleep 0.25
  done
  die "runtime daemon(s) $(echo $remaining) still running after 10s — kill them manually"
}

# cmd_daemon_refresh restarts the daemon ONLY when it is running and STALE, and does nothing
# otherwise. It is what a rebuild calls, so the two facts that cost us a debugging session stop
# recurring: a rebuild that leaves the daemon on a deleted inode, and a rebuild that cycles a daemon
# nobody changed.
#
# WHY NOT ALWAYS `daemon-stop` + `daemon-start`, which is what the report suggested. Stopping
# unconditionally discards the warm container pool and interrupts any in-flight lease — and the daemon
# is HOST-WIDE (one per machine, shared by dev, prod and test), so a `make rebuild-dev` would disturb a
# prod run for no reason. daemon_is_stale already answers the only question that matters: is the daemon
# serving the binary we just built?
#
# THE NO-OPS, each deliberate:
#   not healthy        nothing to refresh, and a REBUILD must not START a daemon the operator did not
#                      have — that is an installer's job, not a build's.
#   healthy and fresh  the common case; say so and move on.
cmd_daemon_refresh() {
  if ! daemon_healthy; then
    log_dim "runtime daemon not running — nothing to refresh"
    return 0
  fi
  if ! daemon_is_stale; then
    log_dim "runtime daemon already serving bin/orchicon — left running"
    return 0
  fi
  log "runtime daemon is serving a STALE binary — restarting it on bin/orchicon"

  # REUSE THE STOP, IN A SUBSHELL. cmd_daemon_stop owns the kill/wait/diagnose sequence; a second copy
  # is how the two would come to disagree about what "stopped" means. The subshell is because it
  # `die`s when a daemon will not go: right for `make runtime-stop` (the operator asked it to stop, so
  # failing to stop IS the answer) and wrong for a REBUILD, whose other work has already succeeded and
  # which must not be failed by a stubborn host process.
  ( cmd_daemon_stop ) || true

  # TRUST THE SOCKET, NOT AN EXIT CODE. Measured while writing this: a stop reported success while the
  # old daemon was still going away, and re-asking the socket is the only claim that means anything.
  if daemon_healthy; then
    log "WARNING: the runtime daemon is still answering after a stop attempt"
    log_dim "  it may still be serving the previous binary; clear it by hand:"
    log_dim "    make runtime-stop && make runtime-daemon"
    return 0
  fi
  cmd_daemon_start
}

cmd_daemon_status() {
  if daemon_healthy; then
    printf 'runtime daemon: UP (%s)\n' "$SOCKET"
  else
    printf 'runtime daemon: DOWN (%s does not answer /v1/health)\n' "$SOCKET"
  fi
  printf '  socket    %s\n' "$SOCKET"
  printf '  log       %s\n' "$DAEMON_LOG"
  printf '  engine    %s\n' "$ENGINE"
  printf '  images    %s %s %s\n' "$BASE_IMAGE" "$GUI_IMAGE" "$DEV_IMAGE"
  printf '  allowlist %s\n' "${ORCHICON_RUNTIME_IMAGES:-<daemon default: base image>}"
  daemon_healthy
}

case "${1:-help}" in
  build)         cmd_build ;;
  daemon-start)  cmd_daemon_start ;;
  daemon-refresh) cmd_daemon_refresh ;;
  daemon-stop)   cmd_daemon_stop ;;
  daemon-status) cmd_daemon_status ;;
  help|-h|--help|"") usage 0 ;;
  *)
    printf 'runtime.sh: unknown command %s\n\n' "$1" >&2
    usage 2
    ;;
esac
