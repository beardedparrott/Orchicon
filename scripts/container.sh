#!/usr/bin/env bash
# scripts/container.sh — manage the single-container Orchicon instances.
#
# The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/
# Grafana, control plane) runs inside ONE container via `orchicon
# container` (the binary is the PID-1 supervisor). This script manages
# two isolated instances — dev and prod (dogfooding) — as two containers
# on offset published ports with separate data volumes.
#
# Usage:
#   scripts/container.sh build                    # build the image
#   scripts/container.sh up [dev|prod]            # start an instance (default dev)
#   scripts/container.sh down [dev|prod]          # stop + remove an instance
#   scripts/container.sh status [dev|prod]        # show instance state
#   scripts/container.sh logs [dev|prod]          # tail an instance's supervisor log
#   scripts/container.sh ps                       # list orchicon containers
#
# Instance layout:
#   dev:  orchicon-cnt-dev   ports 8080:8080, 3002:3000   (plane + Grafana)
#   prod: orchicon-cnt-prod  ports 8091:8080, 3003:3000
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
      ;;
    prod)
      NAME="orchicon-cnt-prod"
      VOLUME="orchicon-cnt-prod-data"
      PG_VOLUME="${ORCHICON_PG_VOLUME:-orchicon-prod_postgres-data}"
      COMPOSE_PG="orchicon-prod-postgres"
      COMPOSE_STACK_SCRIPT="dev-prod.sh"
      PORTS="-p 8091:8080 -p 3003:3000"
      GRAFANA_URL="http://localhost:8091/grafana"
      ;;
    *)
      echo "Unknown instance: $inst (use dev|prod)" >&2
      return 1
      ;;
  esac
}

C_RESET='\033[0m'; C_BOLD='\033[1m'; C_GREEN='\033[32m'; C_DIM='\033[2m'; C_YELLOW='\033[33m'; C_RED='\033[31m'
log_ok()   { echo -e "${C_GREEN}✓${C_RESET} $*"; }
log_dim()  { echo -e "${C_DIM}$*${C_RESET}"; }
log_warn() { echo -e "${C_YELLOW}!${C_RESET} $*"; }
log_err()  { echo -e "${C_RED}✗${C_RESET} $*" >&2; }

build_image() {
  log_dim "Building $IMAGE from $DOCKERFILE…"
  if [ ! -f "$PROJECT_ROOT/bin/orchicon" ]; then
    log_err "bin/orchicon not found — run 'make build' first (builds the frontend-embedded binary)"
    return 1
  fi
  cp "$PROJECT_ROOT/bin/orchicon" "$CONTEXT/orchicon"
  docker build -f "$DOCKERFILE" -t "$IMAGE" "$CONTEXT"
  log_ok "Image $IMAGE built (run 'scripts/container.sh up' to start an instance)"

  # Workflow runtime base image (one short-lived container per active
  # workflow run — see DOCUMENTATION.md §Workflow Runtime Containers).
  local RT_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile"
  local RT_CONTEXT="$PROJECT_ROOT/deploy/runtime"
  log_dim "Building $RUNTIME_IMAGE from $RT_DOCKERFILE…"
  cp "$PROJECT_ROOT/bin/orchicon" "$RT_CONTEXT/orchicon"
  docker build -f "$RT_DOCKERFILE" -t "$RUNTIME_IMAGE" "$RT_CONTEXT"
  log_ok "Runtime image $RUNTIME_IMAGE built"

  # GUI variant of the runtime base (headless GUI libs — PySide6 offscreen,
  # tkinter, browser screenshots). Built FROM the base so the label + chown
  # model are inherited. Tagged "<base-tag>-gui" (e.g. orchicon-runtime:local-gui).
  local RT_GUI_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile.gui"
  if [ -f "$RT_GUI_DOCKERFILE" ]; then
    local GUI_IMAGE="${ORCHICON_RUNTIME_GUI_IMAGE:-orchicon-runtime:local-gui}"
    log_dim "Building $GUI_IMAGE from $RT_GUI_DOCKERFILE (base $RUNTIME_IMAGE)…"
    docker build --build-arg BASE_IMAGE="$RUNTIME_IMAGE" -f "$RT_GUI_DOCKERFILE" -t "$GUI_IMAGE" "$RT_CONTEXT"
    log_ok "Runtime GUI image $GUI_IMAGE built"
  fi

  # Orchicon-dev variant (dogfooding): Go/Node/buf/atlas + baked Postgres so
  # a worker can build and DB-test the Orchicon repo in-sandbox.
  local RT_DEV_DOCKERFILE="$PROJECT_ROOT/deploy/runtime/Dockerfile.dev"
  if [ -f "$RT_DEV_DOCKERFILE" ]; then
    local DEV_IMAGE="${ORCHICON_RUNTIME_DEV_IMAGE:-orchicon-runtime:orchicon-dev}"
    log_dim "Building $DEV_IMAGE from $RT_DEV_DOCKERFILE (base $RUNTIME_IMAGE)…"
    docker build --build-arg BASE_IMAGE="$RUNTIME_IMAGE" -f "$RT_DEV_DOCKERFILE" -t "$DEV_IMAGE" "$RT_CONTEXT"
    log_ok "Runtime dev image $DEV_IMAGE built"
  fi
}

# start_runtime_daemon ensures the host-side runtime orchestrator is
# running. It owns the Docker socket and serves the narrow workflow-
# runtime API over a unix socket (mounted into the supervisor container
# below). Idempotent — no-op when already up.
# The runtime daemon's socket lives inside a bind-mounted DIRECTORY (not a
# single file) so a daemon restart — which recreates the socket file —
# never staleness the supervisor container's mount. The container mounts
# the whole dir at /var/run/orchicon-runtime.
RUNTIME_SOCKET_DIR="${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}"
RUNTIME_SOCKET="${ORCHICON_RUNTIME_SOCKET:-$RUNTIME_SOCKET_DIR/runtime.sock}"
start_runtime_daemon() {
  if [ -S "$RUNTIME_SOCKET" ] && curl -s --unix-socket "$RUNTIME_SOCKET" http://runtime/v1/health >/dev/null 2>&1; then
    log_dim "runtime daemon already up ($RUNTIME_SOCKET)"
    return 0
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

# stop_runtime_daemon removes every runtime container for an instance.
stop_runtime_daemon() {
  local inst="${1:-dev}"
  docker ps -a --filter label=orchicon.workflow --format '{{.Names}}' | while read -r name; do
    docker rm -f "$name" >/dev/null 2>&1 && log_dim "removed runtime $name"
  done
}

# rebuild_image = down -> build -> up for one instance: the one-command
# "stop, build, start" loop for a dev/prod container.
rebuild_image() {
  local inst="${1:-dev}"
  down_instance "$inst"
  build_image
  up_instance "$inst"
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
    if ! docker inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' "$NAME" 2>/dev/null | grep -qx "$pm"; then
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

  log_dim "Starting $inst instance ($NAME)…"

  # Scoped mounts — NOT the whole $HOME:
  #   1. opencode config (read-only) + data/auth (rw) so workers can use
  #      the user's real model providers.
  #   2. project dirs/files from the plane-written manifest on the data
  #      volume (project_dir + context_files). `up` auto-syncs: if the
  #      manifest gained a path the running container lacks, the container
  #      is recreated with the new mount set.
  #   3. any extra paths in ORCHICON_PROJECT_MOUNTS (space-separated).
  local MOUNTS=()
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
  # its mounts already cover the desired project paths; otherwise recreate
  # it with the new mount set.
  if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    local missing=""
    for pm in $project_paths; do
      if ! docker inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' "$NAME" 2>/dev/null | grep -qx "$pm"; then
        missing="$missing $pm"
      fi
    done
    if [ -n "$missing" ]; then
      log_warn "mounts changed ($missing) — recreating $NAME"
      docker rm -f "$NAME" >/dev/null
    else
      docker start "$NAME" >/dev/null
      log_ok "$inst instance started"
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
  docker run -d --name "$NAME" \
    --label orchicon-instance="$inst" \
    ${PORTS} \
    -e ORCHICON_GRAFANA_PUBLIC_URL="$GRAFANA_URL" \
    -e "ORCHICON_HOST_UID=$(id -u)" \
    -e "ORCHICON_HOST_GID=$(id -g)" \
    -e "ORCHICON_HOST_HOME=$HOME" \
    -e "ORCHICON_INSTANCE=$inst" \
    "${MOUNTS[@]}" \
    "$IMAGE" >/dev/null
  log_ok "$inst instance started:"
  echo -e "  Control plane:  ${C_DIM}http://localhost:$(echo "$PORTS" | grep -oP '\d+(?=:8080)')${C_RESET}"
  echo -e "  Grafana:        ${C_DIM}${GRAFANA_URL}${C_RESET}"
  echo -e "  Postgres data:  ${C_DIM}$PG_VOLUME${C_RESET}"
  echo ""
  echo -e "  Wait for health: ${C_DIM}curl http://localhost:$(echo "$PORTS" | grep -oP '\d+(?=:8080)')/healthz${C_RESET}"
  echo -e "  Logs:           ${C_DIM}scripts/container.sh logs $inst${C_RESET}"
  echo -e "  Stop:           ${C_DIM}scripts/container.sh down $inst${C_RESET}"
}

down_instance() {
  local inst="${1:-dev}"
  instance_info "$inst"
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
    if docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
      local state
      state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' "$NAME" 2>/dev/null || echo "running")
      echo -e "  $i: ${C_GREEN}running ($state)${C_RESET} ${C_DIM}$NAME${C_RESET}"
    elif docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
      echo -e "  $i: ${C_YELLOW}stopped${C_RESET} ${C_DIM}$NAME${C_RESET}"
    else
      echo -e "  $i: ${C_RED}not created${C_RESET}"
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
  runtime-daemon) start_runtime_daemon ;;
  runtime-stop)
    # Stop the runtime daemon (and any runtime containers) for an instance.
    stop_runtime_daemon "${2:-dev}"
    pid=$(pgrep -x orchicon | head -1)
    [ -n "$pid" ] && kill "$pid" 2>/dev/null && log_ok "runtime daemon stopped"
    [ -z "$pid" ] && log_dim "runtime daemon not running"
    ;;
  ps)
    docker ps -a --filter label=orchicon-instance --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
    docker ps -a --filter label=orchicon.workflow --format 'table {{.Names}}\t{{.Status}}'
    ;;
  *)
    echo "Usage: $0 {build|rebuild [dev|prod]|sync-mounts [dev|prod]|up [dev|prod]|down [dev|prod]|status [dev|prod]|logs [dev|prod]|ps|runtime-daemon|runtime-stop}"
    exit 1
    ;;
esac
