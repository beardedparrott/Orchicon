#!/usr/bin/env bash
# scripts/container.sh — manage the single-container Orchicon instances.
#
# The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/
# Grafana, control plane) runs inside ONE container via `orchicon
# container` (the binary is the PID-1 supervisor). This script manages
# two isolated instances — dev and prod — as two containers
# on offset published ports with separate data volumes.
#
# Usage:
#   scripts/container.sh build                    # build the image
#   scripts/container.sh up [dev|prod]            # start an instance (default dev)
#   scripts/container.sh down [dev|prod]          # stop + remove an instance
#   scripts/container.sh status [dev|prod]        # show instance state
#   scripts/container.sh logs [dev|prod]          # tail an instance's supervisor log
#   scripts/container.sh ps                       # list orchicon containers
#   scripts/container.sh shape [dev|prod]         # print an instance's launch shape (no side effects)
#   scripts/container.sh verify [dev|prod]        # what the instance ACTUALLY runs (shape + ports + plane)
#   scripts/container.sh plane-start [dev|prod]   # start the HOST plane for a host-resident instance
#   scripts/container.sh plane-stop [dev|prod]    # stop the HOST plane (services container keeps running)
#   scripts/container.sh plane-status [dev|prod]  # host plane PID + health
#
# TWO SHAPES (residency, per instance, OPT-IN):
#   container (default): everything — postgres, nats, telemetry, the control
#                        plane — runs inside the instance's container. Today's
#                        behaviour, unchanged.
#   host:                the container runs the SERVICES ONLY (it is started
#                        with ORCHICON_CONTAINER_SERVICES_ONLY=1) and the
#                        control plane runs on the HOST as a separate
#                        `orchicon serve` process, reaching the services over
#                        loopback ports. Opt in per instance with
#                        ORCHICON_PLANE_RESIDENCY=host (or `make rebuild-dev`).
#
# Instance layout:
#   dev:  orchicon-cnt-dev   ports 8080:8080, 3002:3000   (plane + Grafana)
#   prod: orchicon-cnt-prod  ports 8091:8080, 3003:3000
#
# Host-residency port table (loopback-bound publishes, disjoint per instance):
#   service            dev   prod
#   postgres           5432  5433
#   nats / monitoring  4222  4223  /  8222 8223
#   OTLP gRPC / HTTP   4317  4319  /  4318 4320
#   tempo / loki       3200  3201  /  3100 3101
#   victoriametrics    8428  8429
#   grafana            3002  3003  (→ container 3000)
#   plane HTTP         8080  8091
# No value here may be GLOBAL: a shared port or URL would silently point one
# instance's workers/plane at the other.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

IMAGE="${ORCHICON_IMAGE:-orchicon:local}"
DOCKERFILE="deploy/container/Dockerfile"
CONTEXT="deploy/container"
# Workflow runtime base image (see build_image).
RUNTIME_IMAGE="${ORCHICON_RUNTIME_IMAGE:-orchicon-runtime:local}"
# ORCHICON_PG_VOLUME overrides the Postgres data volume. The default
# reuses the existing compose-stack volumes (orchicon_postgres-data /
# orchicon-prod_postgres-data) so your dev/prod data is preserved when
# switching to the single-container workflow. Set to "fresh" to start
# with an empty database in the instance volume instead.

instance_info() {
  local inst="$1"
  case "$inst" in
    dev)
      NAME="orchicon-cnt-dev"
      VOLUME="orchicon-cnt-dev-data"
      PG_VOLUME="${ORCHICON_PG_VOLUME:-orchicon_postgres-data}"
      COMPOSE_PG="orchicon-postgres"
      COMPOSE_STACK_SCRIPT="dev.sh"
      PORTS="-p 8080:8080 -p 3002:3000"
      GRAFANA_URL="http://localhost:8080/grafana"
      PLANE_HTTP_PORT=8080
      PLANE_PUBLIC_URL="http://172.17.0.1:8080"
      PG_PORT=5432
      NATS_PORT=4222
      NATS_MON_PORT=8222
      OTLP_GRPC_PORT=4317
      OTLP_HTTP_PORT=4318
      TEMPO_PORT=3200
      LOKI_PORT=3100
      VM_PORT=8428
      GRAFANA_HOST_PORT=3002
      HOST_DATA_DIR="$HOME/.local/share/orchicon-dev"
      ;;
    prod)
      NAME="orchicon-cnt-prod"
      VOLUME="orchicon-cnt-prod-data"
      PG_VOLUME="${ORCHICON_PG_VOLUME:-orchicon-prod_postgres-data}"
      COMPOSE_PG="orchicon-prod-postgres"
      COMPOSE_STACK_SCRIPT="dev-prod.sh"
      PORTS="-p 8091:8080 -p 3003:3000"
      GRAFANA_URL="http://localhost:8091/grafana"
      PLANE_HTTP_PORT=8091
      PLANE_PUBLIC_URL="http://172.17.0.1:8091"
      PG_PORT=5433
      NATS_PORT=4223
      NATS_MON_PORT=8223
      OTLP_GRPC_PORT=4319
      OTLP_HTTP_PORT=4320
      TEMPO_PORT=3201
      LOKI_PORT=3101
      VM_PORT=8429
      GRAFANA_HOST_PORT=3003
      HOST_DATA_DIR="$HOME/.local/share/orchicon-prod"
      ;;
    *)
      echo "Unknown instance: $inst (use dev|prod)" >&2
      return 1
      ;;
  esac

  # Host-residency publishes (services-only mode). EVERY port is bound to
  # 127.0.0.1 EXPLICITLY: postgres and nats are a database and an internal
  # event bus, and must never be reachable from the LAN. This loopback publish
  # is the security boundary for the pg_hba trust rules the supervisor adds.
  SERVICE_PORTS="-p 127.0.0.1:$PG_PORT:5432"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$NATS_PORT:4222 -p 127.0.0.1:$NATS_MON_PORT:8222"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$OTLP_GRPC_PORT:4317 -p 127.0.0.1:$OTLP_HTTP_PORT:4318"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$TEMPO_PORT:3200 -p 127.0.0.1:$LOKI_PORT:3100"
  SERVICE_PORTS="$SERVICE_PORTS -p 127.0.0.1:$VM_PORT:8428 -p 127.0.0.1:$GRAFANA_HOST_PORT:3000"
}

# residency_for prints the plane residency for THIS invocation: host or
# container. Default is container — the opt-in default is what makes the
# migration safe: with no new setting, an instance behaves exactly as today.
residency_for() {
  local value="${ORCHICON_PLANE_RESIDENCY:-container}"
  case "$value" in
    host|container)
      echo "$value"
      ;;
    *)
      log_err "ORCHICON_PLANE_RESIDENCY must be 'host' or 'container' (got '$value')"
      return 1
      ;;
  esac
}

# plane_env prints the HOST plane's environment profile for an instance, one
# KEY=VALUE per line. EVERY value is sourced from instance_info, so no value
# can be global: a shared HTTP port or plane URL would silently point one
# instance's workers at the other. Values the operator already set in the
# environment (identity/keys) are inherited rather than invented.
plane_env() {
  local inst="${1:-dev}"
  instance_info "$inst"
  echo "ORCHICON_INSTANCE=$inst"
  echo "ORCHICON_HTTP_ADDR=:$PLANE_HTTP_PORT"
  echo "ORCHICON_PLANE_PUBLIC_URL=$PLANE_PUBLIC_URL"
  echo "ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:$PG_PORT/orchicon?sslmode=disable"
  echo "ORCHICON_NATS_URL=nats://localhost:$NATS_PORT"
  echo "ORCHICON_OTEL_ENDPOINT=localhost:$OTLP_GRPC_PORT"
  echo "ORCHICON_GRAFANA_URL=http://localhost:$GRAFANA_HOST_PORT"
  echo "ORCHICON_TEMPO_URL=http://localhost:$TEMPO_PORT"
  echo "ORCHICON_LOKI_URL=http://localhost:$LOKI_PORT"
  echo "ORCHICON_VM_URL=http://localhost:$VM_PORT"
  # State dir: the KEK and ask-history live here. The host VALUE moves; the
  # expectation (`<DataDir>/secrets/kek`) does not — migrate_host_data_dir
  # copies the container volume so existing tenant secrets still decrypt.
  echo "ORCHICON_DATA_DIR=$HOST_DATA_DIR"
  echo "ORCHICON_BLOB_DIR=$HOST_DATA_DIR/blobs"
  echo "ORCHICON_RUNTIME_SOCKET=${RUNTIME_SOCKET:-${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}/runtime.sock}"
  # Per-instance PID/log file for `serve --detach`/`--stop`: two host planes
  # must never share one.
  echo "ORCHICON_SERVE_STATE_DIR=$HOST_DATA_DIR/serve"
  local inherit
  for inherit in ORCHICON_DEPLOYMENT_TENANT_ID ORCHICON_AUTH_SIGNING_KEY ORCHICON_SECRETS_KEK; do
    if [ -n "${!inherit:-}" ]; then
      echo "$inherit=${!inherit}"
    fi
  done
}

# print_shape dumps ONE instance's resolved launch shape. Echo only — no
# docker, no side effects — so the shape can be asserted without starting
# anything. This is what makes the dev/prod coexistence rule checkable: each
# instance's shape is derived per invocation from instance_info, and a rebuild
# of one instance cannot alter the other's.
print_shape() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local residency
  residency=$(residency_for) || return 1
  echo "instance=$inst"
  echo "container=$NAME"
  echo "volume=$VOLUME"
  echo "residency=$residency"
  echo "plane_http_port=$PLANE_HTTP_PORT"
  echo "plane_public_url=$PLANE_PUBLIC_URL"
  echo "grafana_url=$GRAFANA_URL"
  echo "host_data_dir=$HOST_DATA_DIR"
  echo "service_ports=$SERVICE_PORTS"
  if [ "$residency" = "host" ]; then
    echo "publish_ports=$SERVICE_PORTS"
    echo "container_env=ORCHICON_CONTAINER_SERVICES_ONLY=1"
    local line
    while IFS= read -r line; do
      echo "plane_env:$line"
    done < <(plane_env "$inst")
  else
    echo "publish_ports=$PORTS"
    echo "container_env="
  fi
}

# migrate_host_data_dir performs the ONE-TIME switch-over from a containerized
# plane's data volume to the host path. The KEK lives at
# <data-dir>/secrets/kek: switching ORCHICON_DATA_DIR without carrying it over
# would orphan every tenant secret. An existing host KEK is never overwritten.
migrate_host_data_dir() {
  local inst="${1:-dev}"
  instance_info "$inst"
  if [ -f "$HOST_DATA_DIR/secrets/kek" ]; then
    log_dim "host data dir already carries a KEK ($HOST_DATA_DIR) — leaving it untouched"
    return 0
  fi
  if ! docker volume inspect "$VOLUME" >/dev/null 2>&1; then
    log_dim "no existing data volume ($VOLUME) — starting with a fresh host data dir"
    return 0
  fi
  local contents
  contents=$(docker run --rm -v "$VOLUME:/data" alpine ls -A /data 2>/dev/null || true)
  if [ -z "$contents" ]; then
    log_dim "data volume $VOLUME is empty — nothing to migrate"
    return 0
  fi
  log_warn "first host switch-over for $inst: copying volume $VOLUME to $HOST_DATA_DIR"
  log_dim "  the KEK (secrets/kek) and ask-history live there; the data-dir VALUE moves, its derivation does not"
  mkdir -p "$HOST_DATA_DIR"
  if ! docker run --rm -v "$VOLUME:/data" alpine tar cf - -C /data . | tar xf - -C "$HOST_DATA_DIR"; then
    log_err "could not copy $VOLUME to $HOST_DATA_DIR"
    return 1
  fi
  if [ -f "$HOST_DATA_DIR/secrets/kek" ]; then
    log_ok "copied the instance data; KEK preserved, tenant secrets keep decrypting"
  else
    log_warn "copied, but the volume has no secrets/kek — a NEW KEK will be created"
  fi
}

# host_plane_bin resolves the binary the HOST plane runs. Defaults to the repo
# binary the same rebuild just built, so the resident plane matches the image.
host_plane_bin() {
  echo "${ORCHICON_HOST_BIN:-$PROJECT_ROOT/bin/orchicon}"
}

# plane_start launches the HOST plane for an instance, detached, and waits for
# its /healthz. Idempotent: an already-healthy plane is left alone.
plane_start() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local bin
  bin=$(host_plane_bin)
  if [ ! -x "$bin" ]; then
    log_err "host plane binary missing or not executable: $bin"
    log_err "  build it first (make build) or set ORCHICON_HOST_BIN"
    return 1
  fi
  if curl -fs "http://localhost:$PLANE_HTTP_PORT/healthz" >/dev/null 2>&1; then
    log_ok "$inst host plane already serving http://localhost:$PLANE_HTTP_PORT"
    return 0
  fi
  local envargs=()
  local kv
  while IFS= read -r kv; do
    [ -n "$kv" ] && envargs+=("$kv")
  done < <(plane_env "$inst")
  log_dim "starting the host plane for $inst ($bin)"
  # env -u ORCHICON_CONTAINER_MODE: a stray export in a shell profile must
  # never flip the HOST plane into container semantics — it would then rewrite
  # custom provider URLs (which are already correct on the host).
  if ! env -u ORCHICON_CONTAINER_MODE ${envargs[@]+"${envargs[@]}"} "$bin" serve --detach; then
    log_err "could not start the host plane for $inst"
    return 1
  fi
  local i
  for i in $(seq 1 60); do
    if curl -fs "http://localhost:$PLANE_HTTP_PORT/healthz" >/dev/null 2>&1; then
      log_ok "$inst host plane ready: http://localhost:$PLANE_HTTP_PORT"
      return 0
    fi
    sleep 1
  done
  log_err "$inst host plane did not answer http://localhost:$PLANE_HTTP_PORT/healthz within 60s"
  log_dim "  logs: $HOST_DATA_DIR/serve/logs/orchicon.log"
  return 1
}

# plane_stop stops ONLY this instance's host plane. ORCHICON_SERVE_STATE_DIR
# isolates the PID file per instance, so it can never kill the sibling plane.
# The services container is untouched — that is the whole point of the split.
plane_stop() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local bin
  bin=$(host_plane_bin)
  if [ ! -x "$bin" ]; then
    log_dim "host plane binary missing ($bin) — nothing to stop"
    return 0
  fi
  local envargs=()
  local kv
  while IFS= read -r kv; do
    [ -n "$kv" ] && envargs+=("$kv")
  done < <(plane_env "$inst")
  env -u ORCHICON_CONTAINER_MODE ${envargs[@]+"${envargs[@]}"} "$bin" serve --stop \
    || log_dim "no detached host plane running for $inst"
}

# plane_status prints the host plane's PID/log state and probes its /healthz.
plane_status() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local bin
  bin=$(host_plane_bin)
  if [ ! -x "$bin" ]; then
    log_dim "host plane binary missing ($bin) — no host plane can be running"
    return 1
  fi
  local envargs=()
  local kv
  while IFS= read -r kv; do
    [ -n "$kv" ] && envargs+=("$kv")
  done < <(plane_env "$inst")
  env -u ORCHICON_CONTAINER_MODE ${envargs[@]+"${envargs[@]}"} "$bin" serve --status || true
  if curl -fs "http://localhost:$PLANE_HTTP_PORT/healthz" >/dev/null 2>&1; then
    log_ok "$inst host plane healthy at http://localhost:$PLANE_HTTP_PORT"
    return 0
  fi
  log_warn "$inst host plane is not answering http://localhost:$PLANE_HTTP_PORT/healthz"
  return 1
}

# wait_services_ready blocks until the container accepts postgres and nats
# connections, so the HOST plane never boots its migrations against a
# half-started backend.
wait_services_ready() {
  local inst="${1:-dev}"
  local i
  for i in $(seq 1 60); do
    if docker exec "$NAME" pg_isready -h localhost -p 5432 -U orchicon >/dev/null 2>&1 \
      && docker exec "$NAME" curl -fs http://localhost:8222/healthz >/dev/null 2>&1; then
      log_ok "$NAME services ready (postgres 5432, nats monitor 8222)"
      return 0
    fi
    sleep 1
  done
  log_warn "$NAME services were not confirmed ready within 60s — continuing"
  return 0
}

# verify_instance answers "what does this instance ACTUALLY run?" — the shape,
# the published ports and the services-only env straight from docker, plus the
# host plane's state. This is the dev/prod coexistence assertion.
verify_instance() {
  local inst="${1:-dev}"
  instance_info "$inst"
  local residency
  residency=$(residency_for) || return 1
  echo -e "${C_BOLD}Instance shape: $inst${C_RESET}"
  print_shape "$inst"
  echo
  status_instances "$inst"
  if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    local ports svc_env gate
    ports=$(docker inspect --format '{{range $p, $v := .NetworkSettings.Ports}}{{$p}}={{range $v}}{{.HostIp}}:{{.HostPort}} {{end}}{{end}}' "$NAME" 2>/dev/null || true)
    echo -e "  ${C_DIM}published ports: $ports${C_RESET}"
    svc_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$NAME" 2>/dev/null | grep '^ORCHICON_CONTAINER_SERVICES_ONLY=' || true)
    echo -e "  ${C_DIM}services-only env: ${svc_env:-(unset — the plane runs in this container)}${C_RESET}"
    if [ "$residency" = "host" ]; then
      gate=$(docker logs "$NAME" 2>&1 | grep -m1 'services-only mode' || true)
      echo -e "  ${C_DIM}supervisor log: ${gate:-(services-only line not found yet)}${C_RESET}"
    fi
  fi
  if [ "$residency" = "host" ]; then
    plane_status "$inst" || true
  fi
}

C_RESET='\033[0m'; C_BOLD='\033[1m'; C_GREEN='\033[32m'; C_DIM='\033[2m'; C_YELLOW='\033[33m'; C_RED='\033[31m'
log_ok()   { echo -e "${C_GREEN}✓${C_RESET} $*"; }
log_dim()  { echo -e "${C_DIM}$*${C_RESET}"; }
log_warn() { echo -e "${C_YELLOW}!${C_RESET} $*"; }
log_err()  { echo -e "${C_RED}✗${C_RESET} $*" >&2; }

# sha12 prints the first 12 hex chars of a file's SHA-256 — the "did the
# build inputs change?" signal used to version-gate the stock runtime
# images (same git-tag + content-hash pattern as the rest of Orchicon).
sha12() { sha256sum "$1" | cut -c1-12; }

# runtime_image_version computes the desired version label for a stock
# runtime image: "<app-version>-<dockerfile-sha12>". Embedding the base's
# version in the derived (:gui/:dev) versions gives the rebuild cascade for
# free — a base change alters the derived versions, so they rebuild too.
# $VERSION matches the Makefile's git-tag resolution and stays stable
# between builds within one work item; only a Dockerfile edit (or a new tag)
# changes these versions.
runtime_image_version() {
  local dockerfile="$1" prefix="$2"
  if [ -f "$dockerfile" ]; then
    echo "${prefix}-$(sha12 "$dockerfile")"
  else
    echo ""
  fi
}

# runtime_image_needs_rebuild reports whether a stock runtime image must be
# (re)built: the image is missing, its org.orchicon.runtime.version label
# differs from the desired version, or FORCE_RUNTIME / ORCHICON_FORCE_RUNTIME_REBUILD
# is set. Inspecting the label is cheap (no build), which is the whole point
# of the gate: an unchanged Dockerfile means the label matches and the build
# is skipped, so `make container-build` no longer re-runs the slow :gui/:dev
# installs on every build.
#
# An image that carries org.orchicon.runtime.spec-version was built by the
# runtime daemon from a canned stock row the operator edited + redeployed
# (custom-image build path). It is tenant-owned: container.sh must NEVER
# clobber it with a pristine rebuild, so those images are always skipped.
runtime_image_needs_rebuild() {
  local tag="$1" ver="$2"
  if [ "${FORCE_RUNTIME:-}" = "1" ] || [ "${ORCHICON_FORCE_RUNTIME_REBUILD:-}" = "1" ]; then
    return 0
  fi
  local spec
  spec=$(docker image inspect "$tag" --format '{{index .Config.Labels "org.orchicon.runtime.spec-version"}}' 2>/dev/null || true)
  [ -n "$spec" ] && return 1
  local current
  current=$(docker image inspect "$tag" --format '{{index .Config.Labels "org.orchicon.runtime.version"}}' 2>/dev/null || true)
  [ "$current" != "$ver" ]
}

# buildkit_available reports whether the host docker can run BuildKit
# builds (the docker-buildx plugin resolves). We use BuildKit apt cache
# mounts ONLY when it does; a missing buildx falls back to the classic
# builder, which needs no plugin and already caches an apt layer that
# precedes the changing COPY — so the build never fails over a missing
# cache mount.
buildkit_available() {
  docker buildx version >/dev/null 2>&1
}

# cached_dockerfile writes a transient copy of the given Dockerfile that
# pushes the apt layer onto a persistent BuildKit cache mount
# (--mount=type=cache,target=/var/cache/apt) and echoes the path. It is
# only called when buildkit_available is true; the stock Dockerfile stays
# classic-builder-safe, so a missing buildx degrades to a plain build. The
# apt chains in the shipped Dockerfiles open with `RUN groupmod -g 70
# postgres` (container) or `RUN apt-get update` (runtime) — those are the
# lines the mount is added to. The generated file lives in /tmp and is
# removed by the caller.
cached_dockerfile() {
  local srcfile="$1" tmp
  tmp="$(mktemp)"
  sed -E \
    -e '1i # syntax=docker/dockerfile:1' \
    -e 's/^RUN (groupmod -g 70 postgres|apt-get update) \\$/RUN --mount=type=cache,target=\/var\/cache\/apt \1 \\/' \
    "$srcfile" > "$tmp"
  printf '%s' "$tmp"
}

build_image() {
  log_dim "Building $IMAGE from $DOCKERFILE…"
  if [ ! -f "$PROJECT_ROOT/bin/orchicon" ]; then
    log_err "bin/orchicon not found — run 'make build' first (builds the frontend-embedded binary)"
    return 1
  fi
  cp "$PROJECT_ROOT/bin/orchicon" "$CONTEXT/orchicon"
  if [ ! -f "$PROJECT_ROOT/bin/orch" ]; then
    log_err "bin/orch not found — run 'make build' first (builds both binaries)"
    return 1
  fi
  cp "$PROJECT_ROOT/bin/orch" "$CONTEXT/orch"

  # BuildKit apt cache mounts when the host has buildx; otherwise build the
  # stock Dockerfile with the classic builder. Never hard-require buildx —
  # a missing plugin just means a plain (still layer-cached) build.
  local BUILDX=0
  local MAIN_DF="$DOCKERFILE"
  if buildkit_available; then
    BUILDX=1
    export DOCKER_BUILDKIT=1
    MAIN_DF="$(cached_dockerfile "$DOCKERFILE")"
    log_dim "buildx available — building with BuildKit apt cache mounts"
  fi
  docker build -f "$MAIN_DF" -t "$IMAGE" "$CONTEXT"
  log_ok "Image $IMAGE built (run 'scripts/container.sh up' to start an instance)"
  [ "$BUILDX" = "1" ] && rm -f "$MAIN_DF"

  # Workflow runtime base image (one short-lived container per active
  # workflow run — see DOCUMENTATION.md §Workflow Runtime Containers).
  # The orchicon binary is NOT baked into this image anymore — the runtime
  # daemon bind-mounts its own executable into every container it creates,
  # so a rebuilt bin/orchicon is picked up without an image rebuild. The
  # image content is a pure function of the Dockerfiles, so each variant is
  # version-gated (label org.orchicon.runtime.version) and only rebuilt when
  # its Dockerfile (or, for :gui/:dev, the base it derives from) changed.
  local RT_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile"
  local RT_CONTEXT="$PROJECT_ROOT/deploy/runtime"
  local APP_VERSION="${VERSION:-$(git describe --tags --abbrev=0 2>/dev/null || echo dev)}"
  local RT_BASE_VERSION RT_GUI_VERSION RT_DEV_VERSION
  RT_BASE_VERSION="$(runtime_image_version "$RT_DOCKERFILE" "$APP_VERSION")"

  if runtime_image_needs_rebuild "$RUNTIME_IMAGE" "$RT_BASE_VERSION"; then
    local RT_DF="$RT_DOCKERFILE"
    [ "$BUILDX" = "1" ] && RT_DF="$(cached_dockerfile "$RT_DOCKERFILE")"
    log_dim "Building $RUNTIME_IMAGE from $RT_DOCKERFILE (runtime v$RT_BASE_VERSION)…"
    docker build --label "org.orchicon.runtime.version=$RT_BASE_VERSION" -f "$RT_DF" -t "$RUNTIME_IMAGE" "$RT_CONTEXT"
    log_ok "Runtime image $RUNTIME_IMAGE built"
    [ "$BUILDX" = "1" ] && rm -f "$RT_DF"
  else
    log_dim "Runtime image $RUNTIME_IMAGE up to date (runtime v$RT_BASE_VERSION) — skipping"
  fi

  # GUI variant of the runtime base (headless GUI libs — PySide6 offscreen,
  # tkinter, browser screenshots). Built FROM the base so the label + chown
  # model are inherited. Tagged "<base-tag>-gui" (e.g. orchicon-runtime:local-gui).
  local RT_GUI_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile.gui"
  if [ -f "$RT_GUI_DOCKERFILE" ]; then
    local GUI_IMAGE="${ORCHICON_RUNTIME_GUI_IMAGE:-orchicon-runtime:local-gui}"
    RT_GUI_VERSION="$(runtime_image_version "$RT_GUI_DOCKERFILE" "$RT_BASE_VERSION")"
    if runtime_image_needs_rebuild "$GUI_IMAGE" "$RT_GUI_VERSION"; then
      log_dim "Building $GUI_IMAGE from $RT_GUI_DOCKERFILE (runtime v$RT_GUI_VERSION, base $RUNTIME_IMAGE)…"
      docker build --label "org.orchicon.runtime.version=$RT_GUI_VERSION" --build-arg BASE_IMAGE="$RUNTIME_IMAGE" -f "$RT_GUI_DOCKERFILE" -t "$GUI_IMAGE" "$RT_CONTEXT"
      log_ok "Runtime GUI image $GUI_IMAGE built"
    else
      log_dim "Runtime GUI image $GUI_IMAGE up to date (runtime v$RT_GUI_VERSION) — skipping"
    fi
  fi

  # Orchicon-dev variant: Go/Node/buf/atlas + baked Postgres so
  # a worker can build and DB-test the Orchicon repo in-sandbox.
  local RT_DEV_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile.dev"
  if [ -f "$RT_DEV_DOCKERFILE" ]; then
    local DEV_IMAGE="${ORCHICON_RUNTIME_DEV_IMAGE:-orchicon-runtime:orchicon-dev}"
    RT_DEV_VERSION="$(runtime_image_version "$RT_DEV_DOCKERFILE" "$RT_BASE_VERSION")"
    if runtime_image_needs_rebuild "$DEV_IMAGE" "$RT_DEV_VERSION"; then
      log_dim "Building $DEV_IMAGE from $RT_DEV_DOCKERFILE (runtime v$RT_DEV_VERSION, base $RUNTIME_IMAGE)…"
      docker build --label "org.orchicon.runtime.version=$RT_DEV_VERSION" --build-arg BASE_IMAGE="$RUNTIME_IMAGE" -f "$RT_DEV_DOCKERFILE" -t "$DEV_IMAGE" "$RT_CONTEXT"
      log_ok "Runtime dev image $DEV_IMAGE built"
    else
      log_dim "Runtime dev image $DEV_IMAGE up to date (runtime v$RT_DEV_VERSION) — skipping"
    fi
  fi

  # Every `docker build -t <tag>` above repoints the tag and leaves the
  # previous image dangling. Prune those orphans now so repeated dev
  # builds do not accumulate tens of GB of unreferenced layers. Only
  # dangling (untagged) images are removed — running containers pin their
  # image by ID, so nothing in use is touched. The main `orchicon:local`
  # build always runs (it embeds the binary + frontend), so the prune runs
  # every time; when no runtime variant rebuilt there are no new runtime
  # orphans, but the prune is cheap — the expensive part this work item
  # removes is the rebuilds themselves.
  log_dim "Pruning dangling images from this build…"
  docker image prune -f --filter "dangling=true" >/dev/null
}

# start_runtime_daemon ensures the host-side runtime orchestrator is
# running. It owns the Docker socket and serves the narrow workflow-
# runtime API over a unix socket (mounted into the supervisor container
# below). Idempotent — no-op when already up AND fresh: when the running
# daemon's stable binary copy (the one bind-mounted into every runtime
# container) predates the just-built bin/orchicon, the daemon is restarted
# so the containers it spawns stop replaying pre-fix code — the whole fix
# that makes a plain `make rebuild-dev` pick up new tool behavior without
# anyone remembering to `make runtime-stop && make runtime-daemon`.
# The runtime daemon's socket lives inside a bind-mounted DIRECTORY (not a
# single file) so a daemon restart — which recreates the socket file —
# never staleness the supervisor container's mount. The container mounts
# the whole dir at /var/run/orchicon-runtime.
RUNTIME_SOCKET_DIR="${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}"
RUNTIME_SOCKET="${ORCHICON_RUNTIME_SOCKET:-$RUNTIME_SOCKET_DIR/runtime.sock}"

# daemon_is_stale reports whether a RUNNING runtime daemon is serving an
# OLDER binary than the freshly built bin/orchicon. The daemon copies its
# executable to a STABLE path next to the socket at startup (copySelf,
# cmd/orchicon/runtime.go) and bind-mounts THAT copy read-only into every
# runtime container at /usr/local/bin/orchicon — so the stable copy IS the
# binary every runtime container executes (the MCP sidecar, the supervisor,
# the in-container plane). A byte-difference against the just-built
# bin/orchicon therefore means the running daemon — and every container it
# has ever pooled — replays pre-fix code while the freshly rebuilt control
# plane looks new: the 2026-08-31 failure class where a rebuilt instance
# still spawned runtime containers serving the old toolset. Restarting the
# daemon fixes both halves at once: copySelf refreshes the stable copy AND
# the new daemon's start reset (pool resetPool) removes every existing
# runtime container, so they all re-create with (and key on) the fresh
# binary. A missing stable copy is stale by definition (the next start
# re-creates it); `make clean` deleting bin/orchicon does NOT count as
# stale — the daemon keeps running its own inode and its copy stays valid.
daemon_is_stale() {
  local stable="$RUNTIME_SOCKET_DIR/orchicon" fresh="$PROJECT_ROOT/bin/orchicon"
  [ -f "$fresh" ] || return 1   # nothing fresh to compare — not stale
  [ -f "$stable" ] || return 0  # daemon started without its stable copy — restart to re-create
  ! cmp -s "$fresh" "$stable"
}

start_runtime_daemon() {
  if [ -S "$RUNTIME_SOCKET" ] && curl -s --unix-socket "$RUNTIME_SOCKET" http://runtime/v1/health >/dev/null 2>&1; then
    if daemon_is_stale; then
      log_warn "runtime daemon is running a STALE binary (its bind-mount copy predates bin/orchicon) — restarting it"
      log_dim "  every existing runtime container is reaped and re-warmed from the fresh binary on the next run"
      local _pid
      _pid=$(pgrep -f "orchicon runtime-daemon" | head -1 || true)
      if [ -n "$_pid" ]; then
        kill "$_pid" 2>/dev/null || true
        # Wait for the old daemon to release the socket before re-listening.
        for _ in $(seq 1 20); do
          curl -s --unix-socket "$RUNTIME_SOCKET" http://runtime/v1/health >/dev/null 2>&1 || break
          sleep 0.25
        done
      fi
    else
      log_dim "runtime daemon already up and fresh ($RUNTIME_SOCKET)"
      return 0
    fi
  fi
  log_dim "starting runtime daemon…"
  # Local dev: pin the daemon to the locally-built runtime image (the
  # daemon's default is the GHCR release image for one-command installs).
  # Register the locally-built :gui and :orchicon-dev variants in the
  # daemon's image allowlist so they show up in the work-item dropdown
  # (an operator ORCHICON_RUNTIME_IMAGES overrides the default list).
  local DEFAULT_RUNTIME_IMAGES=""
  local GUI_IMAGE="${ORCHICON_RUNTIME_GUI_IMAGE:-$RUNTIME_IMAGE-gui}"
  local DEV_IMAGE="${ORCHICON_RUNTIME_DEV_IMAGE:-orchicon-runtime:orchicon-dev}"
  for img in "$GUI_IMAGE" "$DEV_IMAGE"; do
    if docker image inspect "$img" >/dev/null 2>&1; then
      DEFAULT_RUNTIME_IMAGES="${DEFAULT_RUNTIME_IMAGES:+$DEFAULT_RUNTIME_IMAGES,}$img"
    fi
  done
  ORCHICON_RUNTIME_IMAGE="${ORCHICON_RUNTIME_IMAGE:-$RUNTIME_IMAGE}" \
  ORCHICON_RUNTIME_IMAGES="${ORCHICON_RUNTIME_IMAGES:-$DEFAULT_RUNTIME_IMAGES}" \
  setsid nohup "$PROJECT_ROOT/bin/orchicon" runtime-daemon </dev/null \
    >/tmp/orchicon-runtime-daemon.log 2>&1 &
  for _ in $(seq 1 20); do
    if [ -S "$RUNTIME_SOCKET" ] && curl -s --unix-socket "$RUNTIME_SOCKET" http://runtime/v1/health >/dev/null 2>&1; then
      log_ok "runtime daemon up ($RUNTIME_SOCKET)"
      return 0
    fi
    sleep 0.25
  done
  log_err "runtime daemon failed to start — see /tmp/orchicon-runtime-daemon.log"
  return 1
}

# stop_runtime_daemon removes every runtime container owned by ONE
# instance. Both instances share one host daemon, so the removal is
# instance-scoped (label=orchicon.instance): a dev rebuild must never
# hard-kill a live prod fire's container mid-run. Killing the DAEMON itself
# (the `runtime-stop` case below) is the only wholesale action — its next
# start resets the pool anyway.
stop_runtime_daemon() {
  local inst="${1:-dev}"
  docker ps -a --filter label=orchicon.workflow --filter "label=orchicon.instance=$inst" --format '{{.Names}}' | while read -r name; do
    docker rm -f "$name" >/dev/null 2>&1 && log_dim "removed runtime $name"
  done
}

# rebuild_image = down -> build -> reap -> up for one instance: the
# one-command "stop, build, start" loop for a dev/prod container. The
# explicit reap after the build is the rebuild-time guarantee: at that
# point this instance's plane is down, so every runtime container it
# owns is an orphan by definition (aborted-run leaks, releases lost
# while the plane was rebuilding) — remove them "just in case" so a
# rebuild never carries leaked containers across a restart. The other
# instance's containers are untouched (instance-scoped filter); a stale
# daemon is restarted by start_runtime_daemon inside up_instance, which
# additionally resets the whole pool.
rebuild_image() {
  local inst="${1:-dev}"
  down_instance "$inst"
  build_image
  stop_runtime_daemon "$inst"
  up_instance "$inst"
}

# path_is_mounted reports whether the container $1 already has the path $2 covered —
# by an EXACT bind of that path, or by a bind of a PARENT directory (a project root).
#
# The parent case is what keeps a project root from causing a pointless container
# re-create on every `up` and `sync-mounts`. With $HOME mounted as a root, a project
# dir under it is fully visible to the plane, but the dir's OWN path never appears in
# the container's mount Sources — so an exact-match test declares it missing, every
# time, forever, and re-creates the container for nothing (killing in-flight runs on
# a running instance).
#
# The "$src"/* case requires a following slash, so /home/me/projects-notes is not
# treated as covered by a /home/me/projects mount.
path_is_mounted() {
  local name="$1" pm="$2" src
  while IFS= read -r src; do
    [ -z "$src" ] && continue
    if [ "$src" = "$pm" ]; then return 0; fi
    case "$pm" in
      "$src"/*) return 0 ;;
    esac
  done < <(docker inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' "$name" 2>/dev/null)
  return 1
}

# Host resolv.conf whose nameservers compute_dns_servers reads. Overridable ONLY
# so the offline harness (scripts/tests/container-dns/) can drive every network
# shape without touching /etc; production always reads /etc/resolv.conf.
RESOLV_CONF="${ORCHICON_RESOLV_CONF:-/etc/resolv.conf}"

# dns_probe_available reports whether any tool exists to interrogate a resolver.
# The distinction between "no tool" and "the resolver does not answer" is the
# entire point of this predicate: a missing dig/nslookup must never be read as a
# dead resolver (see compute_dns_servers, case 4).
dns_probe_available() {
  command -v dig >/dev/null 2>&1 || command -v nslookup >/dev/null 2>&1
}

# resolvers_answer reports success when at least one of the given nameservers
# actually resolves a probe hostname. Probing (rather than assuming) is what lets
# `up` tell "host DNS is authoritative here" from "host DNS is the dead gateway
# of a captive portal".
#
# Uses dig when present; falls back to nslookup. When neither exists this returns
# failure — but that failure means "unknown", NOT "dead". Callers must therefore
# consult dns_probe_available first; see compute_dns_servers for why conflating
# the two would silently repoint every host without bind-utils at public DNS.
resolvers_answer() {
  local probe="${1:-example.com}"; shift
  local s
  if command -v dig >/dev/null 2>&1; then
    for s in "$@"; do
      [ -z "$s" ] && continue
      if dig +short +time=2 +tries=1 "$probe" @"$s" 2>/dev/null | grep -q .; then
        return 0
      fi
    done
    return 1
  fi
  if command -v nslookup >/dev/null 2>&1; then
    for s in "$@"; do
      [ -z "$s" ] && continue
      if nslookup -timeout=2 "$probe" "$s" 2>/dev/null | grep -qE 'Name:'; then
        return 0
      fi
    done
    return 1
  fi
  return 1
}

# compute_dns_servers decides which resolvers to pin into the container.
#   1. ORCHICON_CONTAINER_DNS, if set, wins verbatim (explicit operator intent).
#   2. Loopback/stub/link-local host nameservers are dropped outright and are
#      handled as case 3: they are dead inside a container by construction, so
#      public resolvers are the right answer for them with or without a probe.
#   3. If routable host nameservers remain AND a probe tool exists, keep them
#      when one answers — this preserves corporate/VPN networks where internal
#      DNS is authoritative — and fall back to public resolvers when none does
#      (the captive-portal case, where the host's nameserver is a gateway that
#      refuses UDP/53).
#   4. If routable nameservers remain but there is NO probe tool, keep them
#      UNPROBED. Without dig/nslookup we cannot tell a dead gateway from a
#      healthy-but-unverifiable resolver, and guessing "dead" would silently
#      repoint every host lacking bind-utils at public DNS — breaking the
#      split-horizon internal names that resolve there today. Keeping what Docker
#      would have copied is the fail-safe direction; an operator on a captive
#      portal sets ORCHICON_CONTAINER_DNS explicitly.
#
# PRINTF ONLY: the result is consumed as $(compute_dns_servers), and the log_*
# helpers write to stdout, so logging in here would be captured as a resolver.
#
# The loopback filter is essential, not cosmetic. Docker copies the host's
# resolv.conf at create time, and on a systemd-resolved host that file is the
# stub 127.0.0.53. Probing it FROM THE HOST succeeds (resolved is listening),
# but inside the container 127.0.0.53 is the container's own loopback where
# nothing listens — so a naive "does host DNS answer?" check would happily pin
# a guaranteed-dead resolver into every container on a perfectly good network.
# Loopback/stub addresses are therefore dropped before any probing.
compute_dns_servers() {
  if [ -n "${ORCHICON_CONTAINER_DNS:-}" ]; then
    printf '%s' "$ORCHICON_CONTAINER_DNS"
    return 0
  fi

  local host_dns routable
  host_dns=$(awk '/^nameserver[[:space:]]/{printf "%s ", $2}' "$RESOLV_CONF" 2>/dev/null)

  # drop loopback / resolved-stub / link-local addresses
  routable=$(printf '%s\n' $host_dns | grep -vE '^(127\.|::1$|fe80:|0\.0\.0\.0$)' | tr '\n' ' ')

  # Only a stub resolver is statically known to be dead in-container, so public
  # resolvers are the right answer there even with nothing to probe with (2/3).
  # The //-substitution rather than a bare -z test because the pipeline above
  # leaves a WHITESPACE-ONLY string when resolv.conf lists no usable nameserver,
  # and whitespace-only means "no usable resolvers", not "some".
  if [ -z "${routable// /}" ]; then
    printf '1.1.1.1 8.8.8.8'
    return 0
  fi

  # No probe tool: cannot distinguish dead from unverifiable — do not guess.
  # The host's own resolvers are what Docker would have copied anyway, so this
  # is unchanged behaviour rather than a silent repoint (4).
  if ! dns_probe_available; then
    printf '%s' "$routable"
    return 0
  fi

  if resolvers_answer example.com $routable; then
    printf '%s' "$routable"
    return 0
  fi
  printf '1.1.1.1 8.8.8.8'
}

# dns_args_match reports success when the container's configured nameservers
# (HostConfig.Dns) are exactly the desired set, i.e. the container need not be
# recreated. `docker start` cannot rewrite /etc/resolv.conf, so a container born
# on a network whose gateway refused UDP/53 keeps that dead resolver until it is
# recreated — which is exactly what `up` must do when the desired set changes.
#
# This mirrors the existing mount-change semantics: `up` already recreates the
# container when the desired mounts differ, so a desired-DNS change behaving the
# same way is predictable rather than surprising.
#
# Reads HostConfig via `docker inspect` rather than `docker exec` so it works on
# a STOPPED container too. An empty HostConfig.Dns — the pre-fix state, where
# Docker copied the host's resolv.conf — correctly reports as a mismatch.
dns_args_match() {
  local name="$1" want="$2" have a b
  have=$(docker inspect --format '{{range .HostConfig.Dns}}{{.}} {{end}}' "$name" 2>/dev/null)
  a=$(printf '%s\n' $have | sort -u | tr '\n' ' ')
  b=$(printf '%s\n' $want | sort -u | tr '\n' ' ')
  [ -n "$a" ] && [ "$a" = "$b" ]
}

# sync_mounts compares the desired project mounts (plane-written manifest +
# ORCHICON_PROJECT_MOUNTS) against the running container's mounts and
# rebuilds if any are missing. Docker can't add bind mounts to a running
# container, so this is how "I saved a new project dir in the UI" takes
# effect.
sync_mounts() {
  local inst="${1:-dev}"
  instance_info "$inst"
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
    log_dim "$NAME not running — starting"
    up_instance "$inst"
    return 0
  fi
  local desired=""
  desired=$(docker run --rm -v "$VOLUME:/data" alpine cat /data/project-mounts 2>/dev/null || true)
  desired="$desired ${ORCHICON_PROJECT_MOUNTS:-}"
  local missing=""
  for pm in $desired; do
    [ -z "$pm" ] && continue
    if ! path_is_mounted "$NAME" "$pm"; then
      missing="$missing $pm"
    fi
  done
  if [ -n "$missing" ]; then
    log_warn "missing mounts:$missing — rebuilding $NAME"
    down_instance "$inst"
    up_instance "$inst"
  else
    log_ok "$inst mounts up to date"
  fi
}

up_instance() {
  local inst="${1:-dev}"
  instance_info "$inst"

  # Residency is read PER INVOCATION from the environment (the Makefile passes
  # the target's value down), so rebuilding one instance can never change the
  # other's shape.
  local RESIDENCY
  RESIDENCY=$(residency_for) || return 1

  # Data-safety guard: the default Postgres volume is shared with the
  # compose stack. Two postgres processes on one data dir corrupt it, so
  # refuse to start while the compose postgres for this instance is up.
  if [ "$PG_VOLUME" != "fresh" ]; then
    if docker ps --format '{{.Names}}' | grep -qx "$COMPOSE_PG"; then
      log_err "The compose-stack postgres ($COMPOSE_PG) is running and owns $PG_VOLUME."
      log_err "Stop it first to avoid two postgres processes on one data dir:"
      log_err "  scripts/$COMPOSE_STACK_SCRIPT stop   # stop the compose stack"
      log_err "or start with an empty DB instead: ORCHICON_PG_VOLUME=fresh $0 up $inst"
      return 1
    fi
  fi

  log_dim "Starting $inst instance ($NAME)… (plane residency: $RESIDENCY)"

  # HOST RESIDENCY: the container keeps the SERVICES and publishes them on
  # loopback ports; the plane is a HOST process (plane_start below). The
  # container-residency path further down is untouched — same mounts, same
  # create flags — because the two shapes must not diverge in anything else.
  local SERVICES_ONLY=0
  [ "$RESIDENCY" = "host" ] && SERVICES_ONLY=1
  local PUBLISH_PORTS="$PORTS"
  local EXTRA_RUN_ARGS=()
  local HEALTH_ARGS=()
  if [ "$SERVICES_ONLY" = "1" ]; then
    PUBLISH_PORTS="$SERVICE_PORTS"
    EXTRA_RUN_ARGS+=(-e ORCHICON_CONTAINER_SERVICES_ONLY=1)
    # The image's HEALTHCHECK probes the plane's :8080/healthz, which does not
    # exist here — without this override a perfectly healthy services-only
    # container would report unhealthy forever.
    HEALTH_ARGS+=(--health-cmd "pg_isready -h localhost -p 5432 -U orchicon && curl -fs http://localhost:8222/healthz"
      --health-interval 10s --health-timeout 5s --health-start-period 30s --health-retries 20)
  fi

  # Scoped mounts — deliberately narrow, ARMED OVER THE ROOTS ABOVE:
  #   1. opencode config (read-only) + data/auth (rw) so workers can use
  #      the user's real model providers.
  #   2. project dirs/files from the plane-written manifest on the data
  #      volume (project_dir + context_files). `up` auto-syncs: if the
  #      manifest gained a path the running container lacks, the container
  #      is recreated with the new mount set.
  #   3. any extra paths in ORCHICON_PROJECT_MOUNTS (space-separated).
  local MOUNTS=()
  local GH_TOKEN_ENV=""
  # Resolvers pinned into the container at create time. See compute_dns_servers
  # for why the host's resolv.conf is not a safe default on guest networks.
  local dns_servers
  dns_servers=$(compute_dns_servers)
  log_dim "  container DNS: $dns_servers${ORCHICON_CONTAINER_DNS:+ (ORCHICON_CONTAINER_DNS)}"
  # PROJECT ROOTS — WHERE ORCHICON MAY LOOK, DECLARED ONCE.
  #
  # THE PROBLEM THIS SOLVES: a bind mount cannot be added to a running container,
  # so before this, every new project_dir needed a host-side mount change and a
  # container re-create. "Create a project for the directory I am standing in"
  # therefore did NOT work immediately — the plane could not see the path until
  # someone ran `scripts/container.sh up` or `sync-mounts`, and nothing said so
  # (in container mode validateProjectDir deliberately skips the existence check).
  #
  # A ROOT is mounted at its IDENTICAL host path, so every project_dir under it
  # works the moment it is created, with no restart and no extra command. The
  # operator grants reach by DECLARING A ROOT (once) and then CREATING A PROJECT
  # (per directory) — which is the intended permission model.
  #
  # Default is $HOME, so "anywhere I would plausibly work" works out of the box.
  # Override with a colon- or space-separated list to narrow it:
  #   ORCHICON_PROJECT_ROOTS="/home/me/projects:/srv/work" ... up
  # Set it to "none" to mount no roots at all (only the manifest + scoped mounts).
  #
  # THE MOUNTS BELOW THIS BLOCK COME AFTER THE ROOT ON PURPOSE, and the order is
  # load-bearing rather than cosmetic: where two bind mounts target the same or a
  # NESTED path, the more specific destination wins, so the read-only opencode
  # mounts are re-asserted INSIDE the writable $HOME root and keep their `:ro`.
  # Moving them above the root would SILENTLY make opencode's config and CLI
  # writable by the plane — a quiet loss of a deliberate protection. Docker
  # applies mounts in the order given; do not reorder without re-reading this.
  local ROOTS_RAW="${ORCHICON_PROJECT_ROOTS:-$HOME}"
  if [ "$ROOTS_RAW" = "none" ]; then
    ROOTS_RAW=""
  fi
  # Both separators accepted: the env is documented with colons (PATH-like), but a
  # space-separated value is what ORCHICON_PROJECT_MOUNTS already uses, and people
  # will reach for either.
  local roots
  roots=$(echo "$ROOTS_RAW" | tr ':' ' ')
  for root in $roots; do
    [ -z "$root" ] && continue
    if [ -d "$root" ]; then
      MOUNTS+=("-v" "$root:$root")
      log_dim "  project root mounted: $root"
    else
      log_warn "  project root not on host (skipping): $root"
    fi
  done
  [ -d "$HOME/.config/opencode" ] && MOUNTS+=("-v" "$HOME/.config/opencode:$HOME/.config/opencode:ro")
  [ -d "$HOME/.local/share/opencode" ] && MOUNTS+=("-v" "$HOME/.local/share/opencode:$HOME/.local/share/opencode")
  # Runtime CLI adapter install (read-only) — opencode today. Orchicon never
  # ships the adapter binary in the image; the operator installs it on the
  # host and this mount exposes it to the control plane (and, via the
  # daemon, the runtime containers). This keeps the product redistributable
  # regardless of an adapter's license (Claude Code prohibits bundling).
  [ -d "$HOME/.opencode/bin" ] && MOUNTS+=("-v" "$HOME/.opencode:$HOME/.opencode:ro")
  # Git identity + credentials (credential helper "store" reads
  # ~/.git-credentials) so coding workers can commit, push, and open PRs
  # as the user. Read-only mounts.
  [ -f "$HOME/.gitconfig" ] && MOUNTS+=("-v" "$HOME/.gitconfig:$HOME/.gitconfig:ro")
  [ -f "$HOME/.git-credentials" ] && MOUNTS+=("-v" "$HOME/.git-credentials:$HOME/.git-credentials:ro")
  # GitHub CLI auth + state (read-only) so in-process PR/merge workers and
  # the host opencode serve can run `gh` authenticated.
  [ -d "$HOME/.config/gh" ] && MOUNTS+=("-v" "$HOME/.config/gh:$HOME/.config/gh:ro")
  [ -d "$HOME/.local/share/gh" ] && MOUNTS+=("-v" "$HOME/.local/share/gh:$HOME/.local/share/gh:ro")
  # gh's token often lives in the OS keyring (invisible inside the
  # container); inject the host's effective token so workers can create PRs.
  if command -v gh >/dev/null 2>&1; then
    local _gh_tok
    _gh_tok=$(gh auth token 2>/dev/null || true)
    [ -n "$_gh_tok" ] && GH_TOKEN_ENV="-e GH_TOKEN=$_gh_tok"
  fi

  # Workflow runtime daemon socket directory (host-side process that owns
  # the Docker socket and spawns per-workflow runtime containers). Mounted
  # as a DIRECTORY so the mount survives daemon restarts.
  start_runtime_daemon || return 1
  MOUNTS+=("-v" "$RUNTIME_SOCKET_DIR:/var/run/orchicon-runtime")

  # Desired project paths (manifest + explicit). Skip paths absent on host.
  local project_paths=""
  local manifest
  manifest=$(docker run --rm -v "$VOLUME:/data" alpine cat /data/project-mounts 2>/dev/null || true)
  for pm in $manifest ${ORCHICON_PROJECT_MOUNTS:-}; do
    [ -z "$pm" ] && continue
    if [ -d "$pm" ] || [ -f "$pm" ]; then
      MOUNTS+=("-v" "$pm:$pm")
      project_paths="$project_paths $pm"
    else
      log_warn "  project path not on host (skipping): $pm"
    fi
  done

  # If the container already exists, `up` auto-syncs: start it only when
  # its mounts already cover the desired project paths AND its DNS config
  # still matches what we want; otherwise recreate it. `docker start` cannot
  # rewrite /etc/resolv.conf, so a container created on a network with a dead
  # resolver keeps that dead resolver forever unless it is recreated.
  if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    local missing=""
    for pm in $project_paths; do
      if ! path_is_mounted "$NAME" "$pm"; then
        missing="$missing $pm"
      fi
    done
    if [ -n "$missing" ]; then
      log_warn "mounts changed ($missing) — recreating $NAME"
      docker rm -f "$NAME" >/dev/null
    elif ! dns_args_match "$NAME" "$dns_servers"; then
      log_warn "container DNS differs from desired ($dns_servers) — recreating $NAME"
      docker rm -f "$NAME" >/dev/null
    else
      docker start "$NAME" >/dev/null
      log_ok "$inst instance started"
      if [ "$SERVICES_ONLY" = "1" ]; then
        # The container was already up: make sure its backend is serving and
        # that this instance's HOST plane is running against it.
        wait_services_ready "$inst"
        plane_start "$inst"
        echo -e "  Control plane (host): ${C_DIM}http://localhost:${PLANE_HTTP_PORT}${C_RESET}"
      fi
      return 0
    fi
  fi

  # Create the container. Postgres data volume (preserves dev/prod data
  # from the compose stack).
  if [ "$PG_VOLUME" = "fresh" ]; then
    MOUNTS+=("-v" "$VOLUME:/var/lib/orchicon")
  else
    MOUNTS+=("-v" "$VOLUME:/var/lib/orchicon")
    MOUNTS+=("-v" "$PG_VOLUME:/var/lib/orchicon/postgres")
  fi
  # Run the control plane (and its worker subprocesses) as the HOST user so
  # files created in mounted project dirs are owned by you, not root. The
  # supervisor stays root (it drops postgres to uid 70); only the plane +
  # worker processes run as the host user.
  # DNS for the container. By default Docker copies the HOST's resolv.conf at
  # container-create time. On a captive-portal network (hotel/hotspot/airplane)
  # the host's nameserver is the gateway, which commonly REFUSES or blackholes
  # UDP/53 pre-auth. The container then inherits a dead resolver and cannot
  # resolve any AI provider hostname — every provider call fails with a DNS
  # timeout even though raw-IP egress and NAT are perfectly healthy.
  #
  # Pinning public resolvers here is correct for this container's job (reaching
  # provider APIs). Portal sign-in remains the HOST's concern, not the
  # container's. Override with ORCHICON_CONTAINER_DNS="1.1.1.1 8.8.8.8".
  local DNS_ARGS=""
  for _d in $dns_servers; do
    DNS_ARGS="$DNS_ARGS --dns $_d"
  done
  # Short timeouts so a dead resolver fails fast instead of hanging the plane.
  DNS_ARGS="$DNS_ARGS --dns-opt timeout:2 --dns-opt attempts:2"

  docker run -d --name "$NAME" \
    --label orchicon-instance="$inst" \
    --log-driver json-file \
    --log-opt max-size=100m \
    --log-opt max-file=7 \
    $DNS_ARGS \
    ${PUBLISH_PORTS} \
    ${HEALTH_ARGS[@]+"${HEALTH_ARGS[@]}"} \
    -e ORCHICON_GRAFANA_PUBLIC_URL="$GRAFANA_URL" \
    -e "ORCHICON_HOST_UID=$(id -u)" \
    -e "ORCHICON_HOST_GID=$(id -g)" \
    -e "ORCHICON_HOST_HOME=$HOME" \
    -e "ORCHICON_INSTANCE=$inst" \
    ${EXTRA_RUN_ARGS[@]+"${EXTRA_RUN_ARGS[@]}"} \
    ${GH_TOKEN_ENV:-} \
    "${MOUNTS[@]}" \
    "$IMAGE" >/dev/null
  log_ok "$inst instance started:"
  if [ "$SERVICES_ONLY" = "1" ]; then
    wait_services_ready "$inst"
    plane_start "$inst"
  fi
  echo -e "  Control plane:  ${C_DIM}http://localhost:${PLANE_HTTP_PORT}${C_RESET}"
  echo -e "  Grafana:        ${C_DIM}${GRAFANA_URL}${C_RESET}"
  echo -e "  Postgres data:  ${C_DIM}$PG_VOLUME${C_RESET}"
  if [ "$SERVICES_ONLY" = "1" ]; then
    echo ""
    echo -e "  Plane residency: ${C_BOLD}host${C_RESET} ${C_DIM}(the services run in the container; the plane runs on this host)" 
    echo -e "  Services (loopback only): ${C_DIM}$SERVICE_PORTS${C_RESET}"
    echo -e "  Host data dir:  ${C_DIM}$HOST_DATA_DIR${C_RESET}"
    echo -e "  Plane controls: ${C_DIM}scripts/container.sh plane-stop $inst${C_RESET}"
  fi
  echo ""
  echo -e "  Wait for health: ${C_DIM}curl http://localhost:${PLANE_HTTP_PORT}/healthz${C_RESET}"
  echo -e "  Logs:           ${C_DIM}scripts/container.sh logs $inst${C_RESET}"
  echo -e "  Stop:           ${C_DIM}scripts/container.sh down $inst${C_RESET}"
}

down_instance() {
  local inst="${1:-dev}"
  instance_info "$inst"
  # Host residency: the plane is a HOST process, not a container child, so
  # stopping the instance must stop it explicitly. The PID-file test also
  # covers switching an instance BACK to container residency — a leftover host
  # plane would hold the plane port and block the container's publish. (The
  # inverse is a feature: `plane-stop` leaves the services container running.)
  if [ "$(residency_for 2>/dev/null || echo container)" = "host" ] \
    || [ -f "$HOST_DATA_DIR/serve/pids/orchicon.pid" ]; then
    plane_stop "$inst"
  fi
  if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    docker rm -f "$NAME" >/dev/null
    log_ok "$inst instance stopped and removed (data volume $VOLUME preserved)"
  else
    log_dim "$inst instance is not running"
  fi
  stop_runtime_daemon "$inst"
}

status_instances() {
  local inst="${1:-}"
  echo -e "${C_BOLD}Orchicon container instances${C_RESET}"
  local instances
  if [ -n "$inst" ]; then
    instances="$inst"
  else
    instances="dev prod"
  fi
  for i in $instances; do
    instance_info "$i"
    local residency_i
    residency_i=$(residency_for 2>/dev/null || echo container)
    if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
      local state
      state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' "$NAME" 2>/dev/null || echo "running")
      echo -e "  $i: ${C_GREEN}running ($state)${C_RESET} ${C_DIM}$NAME · plane:$residency_i${C_RESET}"
    elif docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
      echo -e "  $i: ${C_YELLOW}stopped${C_RESET} ${C_DIM}$NAME · plane:$residency_i${C_RESET}"
    else
      echo -e "  $i: ${C_RED}not created${C_RESET} ${C_DIM}plane:$residency_i${C_RESET}"
    fi
    if [ "$residency_i" = "host" ]; then
      # The plane is a host process: its state is NOT the container's state.
      plane_status "$i" || true
    fi
  done
}

logs_instance() {
  local inst="${1:-dev}"
  instance_info "$inst"
  docker logs -f "$NAME"
}

case "${1:-}" in
  build) build_image ;;
  rebuild) rebuild_image "${2:-dev}" ;;
  sync-mounts) sync_mounts "${2:-dev}" ;;
  up) up_instance "${2:-dev}" ;;
  down) down_instance "${2:-dev}" ;;
  status) status_instances "${2:-}" ;;
  logs) logs_instance "${2:-dev}" ;;
  shape) print_shape "${2:-dev}" ;;
  verify) verify_instance "${2:-dev}" ;;
  plane-start) plane_start "${2:-dev}" ;;
  plane-stop) plane_stop "${2:-dev}" ;;
  plane-status) plane_status "${2:-dev}" ;;
  runtime-daemon) start_runtime_daemon ;;
  runtime-stop)
    # Stop the runtime daemon (and any runtime containers) for an instance.
    stop_runtime_daemon "${2:-dev}"
    # Match ONLY the host daemon (pgrep -x orchicon would also match the
    # container supervisor/plane, which run in the shared pid namespace).
    # The pattern is unique enough not to match this script's own command
    # line (which never contains "orchicon runtime-daemon").
    pid=$(pgrep -f "orchicon runtime-daemon" | head -1)
    if [ -n "$pid" ]; then
      # Idempotent: a daemon that already exited (or a lost kill) is still a
      # successful "stopped" — stop commands must return 0 so `make
      # runtime-stop && make runtime-daemon` chains (the previous trailing
      # `[ -z "$pid" ]` evaluated false on the stopped path and returned 1).
      kill "$pid" 2>/dev/null || true
      log_ok "runtime daemon stopped"
    else
      log_dim "runtime daemon not running"
    fi
    ;;
  ps)
    docker ps -a --filter label=orchicon-instance --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
    docker ps -a --filter label=orchicon.workflow --format 'table {{.Names}}\t{{.Status}}'
    ;;
  *)
    echo "Usage: $0 {build|rebuild [dev|prod]|sync-mounts [dev|prod]|up [dev|prod]|down [dev|prod]|status [dev|prod]|logs [dev|prod]|shape [dev|prod]|verify [dev|prod]|plane-start|plane-stop|plane-status [dev|prod]|ps|runtime-daemon|runtime-stop}"
    echo "  ORCHICON_PLANE_RESIDENCY=host|container  (default container) picks the shape per invocation."
    exit 1
    ;;
esac
