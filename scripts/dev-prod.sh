#!/usr/bin/env bash
# dev-prod.sh — Production-like Orchicon instance for dogfooding.
#
# Manages a fully isolated Orchicon stack (Docker Compose + control plane)
# alongside the normal dev environment. Uses separate ports, volumes, and
# binary path so it is never affected by dev-instance restarts.
#
# Usage:
#   scripts/dev-prod.sh start     Start production stack
#   scripts/dev-prod.sh stop      Stop everything
#   scripts/dev-prod.sh status    Show status
#   scripts/dev-prod.sh restart   Stop then start
#   scripts/dev-prod.sh logs      Tail control plane logs
#
# Environment:
#   ORCHICON_BIN        Path to the orchicon binary (default: ~/.local/bin/orchicon)
#   ORCHICON_PROD_DIR   State directory (default: .dev/prod)
#
# Ports (offset +1 from dev):
#   HTTP:      :8090     (dev :8080)
#   gRPC:      :9091     (dev :9090)
#   Postgres:  :5433     (dev :5432)
#   NATS:      :4223     (dev :4222)
#   SigNoz:    :3302     (dev :3301)
#   OTel:      :4319     (dev :4317)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

COMPOSE_FILE="deploy/compose/docker-compose-prod.yml"
COMPOSE_PROJECT="orchicon-prod"
COMPOSE="docker compose -f $COMPOSE_FILE -p $COMPOSE_PROJECT"

PROD_DIR="${ORCHICON_PROD_DIR:-.dev/prod}"
PID_DIR="$PROD_DIR/pids"
LOG_DIR="$PROD_DIR/logs"
mkdir -p "$PID_DIR" "$LOG_DIR"

ORCHICON_BIN="${ORCHICON_BIN:-$HOME/.local/bin/orchicon}"

# Prod instance environment: all ports offset +1 from dev defaults.
export ORCHICON_HTTP_ADDR="${ORCHICON_HTTP_ADDR:-:8090}"
export ORCHICON_GRPC_ADDR="${ORCHICON_GRPC_ADDR:-:9091}"
export ORCHICON_POSTGRES_DSN="${ORCHICON_POSTGRES_DSN:-postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable}"
export ORCHICON_NATS_URL="${ORCHICON_NATS_URL:-nats://localhost:4223}"
export ORCHICON_OTEL_ENDPOINT="${ORCHICON_OTEL_ENDPOINT:-localhost:4319}"
export ORCHICON_SIGNOZ_URL="${ORCHICON_SIGNOZ_URL:-http://localhost:3302}"
export ORCHICON_CLICKHOUSE_DSN="${ORCHICON_CLICKHOUSE_DSN:-http://signoz:signoz@localhost:8124}"
export ORCHICON_BLOB_DIR="${ORCHICON_BLOB_DIR:-./data/prod-blobs}"

# --- Colors ---------------------------------------------------------------
if [ -t 1 ]; then
  C_RESET='\033[0m'; C_BOLD='\033[1m'; C_GREEN='\033[32m'
  C_YELLOW='\033[33m'; C_RED='\033[31m'; C_CYAN='\033[36m'; C_DIM='\033[2m'
else
  C_RESET=''; C_BOLD=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_CYAN=''; C_DIM=''
fi

log()     { echo -e "${C_CYAN}▸${C_RESET} $*"; }
log_ok()  { echo -e "${C_GREEN}✓${C_RESET} $*"; }
log_warn(){ echo -e "${C_YELLOW}!${C_RESET} $*"; }
log_err() { echo -e "${C_RED}✗${C_RESET} $*" >&2; }
log_dim() { echo -e "${C_DIM}  $*${C_RESET}"; }

is_running() {
  local pidfile="$1"
  [ -f "$pidfile" ] || return 1
  local pid; pid="$(cat "$pidfile" 2>/dev/null || echo '')"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

stop_pid() {
  local pidfile="$1"
  local name="$2"
  if is_running "$pidfile"; then
    local pid; pid="$(cat "$pidfile")"
    log "stopping $name (pid $pid)…"
    kill "$pid" 2>/dev/null || true
    for i in $(seq 1 10); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    if kill -0 "$pid" 2>/dev/null; then
      log_warn "$name did not exit gracefully, sending SIGKILL"
      kill -9 "$pid" 2>/dev/null || true
    fi
    log_ok "$name stopped"
  else
    log_dim "$name is not running"
  fi
  rm -f "$pidfile"
}

wait_for_http() {
  local url="$1"
  local name="$2"
  local max="${3:-60}"
  for i in $(seq 1 "$max"); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_container() {
  local service="$1"
  local max="${2:-60}"
  for i in $(seq 1 "$max"); do
    local status
    status="$($COMPOSE ps --format json "$service" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('Health','unknown'))" 2>/dev/null || echo 'unknown')"
    if [ "$status" = "healthy" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# --- Actions --------------------------------------------------------------

do_start() {
  echo -e "${C_BOLD}Starting Orchicon production-like instance…${C_RESET}"
  echo "  Project:   $COMPOSE_PROJECT"
  echo "  Compose:   $COMPOSE_FILE"
  echo "  Binary:    $ORCHICON_BIN"
  echo "  HTTP:      $ORCHICON_HTTP_ADDR"
  echo "  Postgres:  $ORCHICON_POSTGRES_DSN"
  echo "  State:     $PROD_DIR"
  echo ""

  # 1. Check if already running.
  local pidfile="$PID_DIR/orchicon.pid"
  if is_running "$pidfile"; then
    log_err "orchicon prod is already running (PID $(cat "$pidfile"))"
    log_err "Run 'scripts/dev-prod.sh stop' first, or 'scripts/dev-prod.sh restart'"
    return 1
  fi

  # 2. Start compose stack.
  log "starting Docker Compose stack…"
  if ! $COMPOSE up -d; then
    log_err "Compose up failed"
    return 1
  fi

  log "waiting for containers to be healthy…"
  local services=("postgres" "nats" "clickhouse")
  for svc in "${services[@]}"; do
    if wait_for_container "$svc" 120; then
      log_ok "$svc is healthy"
    else
      log_warn "$svc did not become healthy in time"
      log_warn "run '$COMPOSE logs $svc' for details"
    fi
  done

  # 3. Apply migrations.
  log "applying migrations…"
  if command -v atlas >/dev/null 2>&1; then
    (cd db && atlas migrate apply --env local --url "$ORCHICON_POSTGRES_DSN") 2>&1 | tail -5
    log_ok "migrations applied"
  else
    log_warn "atlas not on PATH — skipping migrations (run 'make tools' to install)"
  fi

  # 4. Verify binary exists.
  if [ ! -x "$ORCHICON_BIN" ]; then
    log_err "binary not found or not executable: $ORCHICON_BIN"
    log_err "Set ORCHICON_BIN or build & install first: make build && cp ./bin/orchicon $ORCHICON_BIN"
    return 1
  fi

  # 5. Start control plane in background.
  local logfile="$LOG_DIR/orchicon.log"
  log "starting control plane via 'orchicon serve'…"
  nohup "$ORCHICON_BIN" serve >"$logfile" 2>&1 &
  echo $! > "$pidfile"

  # 6. Wait for healthz.
  local health_url="http://localhost${ORCHICON_HTTP_ADDR}/healthz"
  if wait_for_http "$health_url" "control plane" 30; then
    log_ok "control plane is serving (PID $(cat "$pidfile"), log: $logfile)"
  else
    log_err "control plane did not become healthy — check $logfile"
    tail -10 "$logfile" 2>/dev/null || true
    stop_pid "$pidfile" "control plane"
    return 1
  fi

  echo ""
  echo -e "${C_GREEN}${C_BOLD}Orchicon prod instance is running.${C_RESET}"
  echo -e "  Control plane:  ${C_DIM}http://localhost${ORCHICON_HTTP_ADDR}${C_RESET}"
  echo -e "  SigNoz UI:      ${C_DIM}http://localhost:3302${C_RESET}"
  echo -e "  NATS monitor:   ${C_DIM}http://localhost:8223${C_RESET}"
  echo ""
  echo -e "  Logs:           ${C_DIM}tail -f $logfile${C_RESET}"
  echo -e "  Stop:           ${C_DIM}scripts/dev-prod.sh stop${C_RESET}"
}

do_stop() {
  echo -e "${C_BOLD}Stopping Orchicon production-like instance…${C_RESET}"
  stop_pid "$PID_DIR/orchicon.pid" "control plane"
  log "stopping Docker Compose stack…"
  $COMPOSE down 2>/dev/null || log_warn "compose down failed"
  log_ok "prod stack stopped"
  echo ""
  echo -e "${C_YELLOW}Orchicon prod instance is stopped.${C_RESET}"
}

do_status() {
  echo -e "${C_BOLD}Orchicon prod instance status${C_RESET}"
  echo ""

  if is_running "$PID_DIR/orchicon.pid"; then
    local pid; pid="$(cat "$PID_DIR/orchicon.pid")"
    if curl -sf "http://localhost${ORCHICON_HTTP_ADDR}/healthz" >/dev/null 2>&1; then
      log_ok "control plane   ${C_DIM}running (pid $pid) ${C_GREEN}healthy${C_RESET}"
    else
      log_warn "control plane   ${C_DIM}running (pid $pid) ${C_YELLOW}not responding${C_RESET}"
    fi
  else
    log_err "control plane   ${C_DIM}stopped${C_RESET}"
  fi

  echo ""
  echo -e "${C_BOLD}Docker Compose services ($COMPOSE_PROJECT):${C_RESET}"
  $COMPOSE ps --format "table {{.Name}}\t{{.Service}}\t{{.Status}}" 2>/dev/null || log_err "docker compose unavailable"

  echo ""
  echo -e "${C_BOLD}Endpoints:${C_RESET}"
  local ep_url ep_label
  while IFS='|' read -r ep_url ep_label; do
    if curl -sf "$ep_url" >/dev/null 2>&1; then
      log_ok "$ep_label   ${C_DIM}$ep_url ${C_GREEN}ok${C_RESET}"
    else
      log_err "$ep_label   ${C_DIM}$ep_url ${C_RED}unreachable${C_RESET}"
    fi
  done <<ENDPOINTS
http://localhost${ORCHICON_HTTP_ADDR}/healthz|Control plane
http://localhost:3302/|SigNoz
http://localhost:8223/healthz|NATS
ENDPOINTS
}

do_logs() {
  local logfile="$LOG_DIR/orchicon.log"
  if [ ! -f "$logfile" ]; then
    log_err "log file not found: $logfile"
    log_err "Is the prod instance running? Run 'scripts/dev-prod.sh start' first."
    return 1
  fi
  log "tailing control plane logs (Ctrl+C to stop)…"
  tail -f "$logfile"
}

do_restart() {
  do_stop
  echo ""
  sleep 1
  do_start
}

# --- Main ------------------------------------------------------------------
case "${1:-}" in
  start)   do_start ;;
  stop)    do_stop ;;
  status)  do_status ;;
  restart) do_restart ;;
  logs)    do_logs ;;
  *)
    echo "Usage: $0 {start|stop|status|restart|logs}"
    echo ""
    echo "Commands:"
    echo "  start    Start the production-like Orchicon instance"
    echo "  stop     Stop it"
    echo "  status   Show status of all components"
    echo "  restart  Stop then start"
    echo "  logs     Tail control-plane logs"
    exit 1
    ;;
esac
