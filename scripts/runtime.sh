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
BASE_IMAGE="${ORCHICON_RUNTIME_IMAGE:-orchicon-runtime:local}"
GUI_IMAGE="${ORCHICON_RUNTIME_GUI_IMAGE:-orchicon-runtime:local-gui}"
DEV_IMAGE="${ORCHICON_RUNTIME_DEV_IMAGE:-orchicon-runtime:orchicon-dev}"

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

daemon_pid() { pgrep -f 'orchicon runtime-daemon' | head -1 || true; }

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
  local pid
  pid="$(daemon_pid)"
  if [ -z "$pid" ]; then
    log_dim "no runtime daemon is running"
    return 0
  fi
  log "stopping runtime daemon (pid $pid)"
  kill "$pid" 2>/dev/null || true
  local _i
  for _i in $(seq 1 40); do
    [ -z "$(daemon_pid)" ] && { log "runtime daemon stopped"; return 0; }
    sleep 0.25
  done
  die "runtime daemon (pid $pid) is still running after 10s — kill it manually"
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
  daemon-stop)   cmd_daemon_stop ;;
  daemon-status) cmd_daemon_status ;;
  help|-h|--help|"") usage 0 ;;
  *)
    printf 'runtime.sh: unknown command %s\n\n' "$1" >&2
    usage 2
    ;;
esac
