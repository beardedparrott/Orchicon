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
  if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
    log_warn "$NAME already exists (restarting)"
    docker start "$NAME" >/dev/null
    log_ok "$inst instance started"
    return 0
  fi

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
  local MOUNTS=()
  # Postgres data volume (preserves dev/prod data from the compose stack).
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
  docker_run_args=(-e "ORCHICON_HOST_UID=$(id -u)" -e "ORCHICON_HOST_GID=$(id -g)" -e "ORCHICON_HOST_HOME=$HOME")
  # Scoped mounts — NOT the whole $HOME:
  #   1. opencode config (read-only) + data/auth (rw) so workers can use
  #      the user's real model providers.
  #   2. project dirs/files from the plane-written manifest on the data
  #      volume (project_dir + context_files — see DOCUMENTATION.md
  #      §Single-Container Deployment). Save a project dir in the UI, then
  #      run `scripts/container.sh sync-mounts` to apply.
  #   3. any extra paths in ORCHICON_PROJECT_MOUNTS (space-separated).
  [ -d "$HOME/.config/opencode" ] && MOUNTS+=("-v" "$HOME/.config/opencode:$HOME/.config/opencode:ro")
  [ -d "$HOME/.local/share/opencode" ] && MOUNTS+=("-v" "$HOME/.local/share/opencode:$HOME/.local/share/opencode")
  while IFS= read -r pm; do
    [ -z "$pm" ] && continue
    if [ -d "$pm" ] || [ -f "$pm" ]; then
      MOUNTS+=("-v" "$pm:$pm")
    else
      log_warn "  project path from manifest not on host (skipping): $pm"
    fi
  done < <(docker run --rm -v "$VOLUME:/data" alpine cat /data/project-mounts 2>/dev/null || true)
  for pm in ${ORCHICON_PROJECT_MOUNTS:-}; do
    if [ -d "$pm" ] || [ -f "$pm" ]; then
      MOUNTS+=("-v" "$pm:$pm")
    else
      log_warn "  ORCHICON_PROJECT_MOUNTS path not on host (skipping): $pm"
    fi
  done
  docker run -d --name "$NAME" \
    --label orchicon-instance="$inst" \
    ${PORTS} \
    -e ORCHICON_GRAFANA_PUBLIC_URL="$GRAFANA_URL" \
    "${docker_run_args[@]}" \
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
  ps)
    docker ps -a --filter label=orchicon-instance --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
    ;;
  *)
    echo "Usage: $0 {build|rebuild [dev|prod]|sync-mounts [dev|prod]|up [dev|prod]|down [dev|prod]|status [dev|prod]|logs [dev|prod]|ps}"
    exit 1
    ;;
esac
