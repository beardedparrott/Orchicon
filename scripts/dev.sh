#!/usr/bin/env bash
# Orchicon development control script.
#
# Manages the full local development stack: Docker Compose services
# (Postgres, NATS, SigNoz, OTel), the Go control plane, and the Vite
# frontend dev server. One command to get everything running or stopped.
#
# Usage:
#   scripts/dev.sh start     Start everything (dev stack → migrations → control plane → frontend)
#   scripts/dev.sh stop      Stop everything (control plane → frontend → dev stack)
#   scripts/dev.sh status    Show status of all components
#   scripts/dev.sh restart   Stop then start
#   scripts/dev.sh logs      Tail control-plane + frontend logs
#
# PID files live in .dev/ so they don't pollute the repo root.
# Logs live in .dev/ as well.
#
# Every new phase that adds a runtime component (reconciler, adapter,
# recovery engine, etc.) MUST update this script so `dev.sh start`
# brings up the full system. See AGENTS.md §Dev Control Script.
set -euo pipefail

# --- Paths ------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

COMPOSE_FILE="deploy/compose/docker-compose.yml"
DEV_DIR=".dev"
PID_DIR="$DEV_DIR/pids"
LOG_DIR="$DEV_DIR/logs"
COMPOSE="docker compose -f $COMPOSE_FILE"
DB_URL="${ORCHICON_POSTGRES_DSN:-postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable}"

# --- Binary delegation ------------------------------------------------------
# If the orchicon binary is available (built or installed), delegate to
# `orchicon dev` which embeds the compose stack, migrations, and frontend
# bundle — the complete one-binary experience (AGENTS.md §Dev Control
# Script).
ORCHICON_BIN="${ORCHICON_BIN:-./bin/orchicon}"
if [ "${1:-}" != "--no-binary" ] && command -v "$ORCHICON_BIN" >/dev/null 2>&1; then
  case "${1:-}" in
    start|stop|status|restart|logs)
      exec "$ORCHICON_BIN" dev "$@"
      ;;
  esac
fi

# --- Colors -----------------------------------------------------------------
if [ -t 1 ]; then
  C_RESET='\033[0m'
  C_BOLD='\033[1m'
  C_GREEN='\033[32m'
  C_YELLOW='\033[33m'
  C_RED='\033[31m'
  C_CYAN='\033[36m'
  C_DIM='\033[2m'
else
  C_RESET=''; C_BOLD=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_CYAN=''; C_DIM=''
fi

# --- Helpers ----------------------------------------------------------------
mkdir -p "$PID_DIR" "$LOG_DIR"

log()     { echo -e "${C_CYAN}▸${C_RESET} $*"; }
log_ok()  { echo -e "${C_GREEN}✓${C_RESET} $*"; }
log_warn(){ echo -e "${C_YELLOW}!${C_RESET} $*"; }
log_err() { echo -e "${C_RED}✗${C_RESET} $*" >&2; }

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
    # Wait up to 5s for graceful shutdown
    for i in $(seq 1 10); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    # Force kill if still alive
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

log_dim(){ echo -e "${C_DIM}  $*${C_RESET}"; }

wait_for_http() {
  local url="$1"
  local name="$2"
  local max="${3:-30}"
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
    local status; status="$($COMPOSE ps --format json "$service" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('Health','unknown'))" 2>/dev/null || echo 'unknown')"
    if [ "$status" = "healthy" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# --- Actions ----------------------------------------------------------------

start_stack() {
  log "starting dev stack (Docker Compose)…"
  $COMPOSE up -d
  log "waiting for containers to be healthy…"
  local services=("postgres" "nats")
  for svc in "${services[@]}"; do
    if wait_for_container "$svc" 60; then
      log_ok "$svc is healthy"
    else
      log_err "$svc did not become healthy in time"
      log_warn "run '$COMPOSE logs $svc' for details"
      return 1
    fi
  done
  # SigNoz + ClickHouse + OTel are healthy but not required for core dev
  log_ok "dev stack is up (Postgres, NATS ready; SigNoz/OTel may still be starting)"
}

start_migrations() {
  log "applying migrations…"
  if command -v atlas >/dev/null 2>&1; then
    (cd db && atlas migrate apply --env local --url "$DB_URL") 2>&1 | tail -5
    log_ok "migrations applied"
  else
    log_warn "atlas not on PATH — skipping migrations (run 'make tools' to install)"
  fi
}

start_control_plane() {
  local pidfile="$PID_DIR/orchicon.pid"
  local logfile="$LOG_DIR/orchicon.log"
  if is_running "$pidfile"; then
    log_dim "control plane is already running (pid $(cat "$pidfile"))"
    return 0
  fi
  log "building control plane…"
  go build -o bin/orchicon ./cmd/orchicon
  log "starting control plane…"
  nohup ./bin/orchicon >"$logfile" 2>&1 &
  echo $! > "$pidfile"
  # Wait for healthz
  if wait_for_http "http://localhost:8080/healthz" "control plane" 15; then
    log_ok "control plane is serving (pid $(cat "$pidfile"), log: $logfile)"
  else
    log_err "control plane did not become healthy — check $logfile"
    tail -10 "$logfile" 2>/dev/null || true
    return 1
  fi
}

start_frontend() {
  local pidfile="$PID_DIR/vite.pid"
  local logfile="$LOG_DIR/vite.log"
  if is_running "$pidfile"; then
    log_dim "frontend is already running (pid $(cat "$pidfile"))"
    return 0
  fi
  if [ ! -d "frontend/node_modules" ]; then
    log "installing frontend dependencies…"
    (cd frontend && npm install)
  fi
  log "starting Vite dev server…"
  nohup bash -c 'cd frontend && npx vite --host --port 5173' >"$logfile" 2>&1 &
  echo $! > "$pidfile"
  if wait_for_http "http://localhost:5173/" "frontend" 30; then
    log_ok "frontend is serving (pid $(cat "$pidfile"), log: $logfile)"
  else
    log_warn "frontend did not respond in time — it may still be starting (check $logfile)"
  fi
}

do_start() {
  echo -e "${C_BOLD}Starting Orchicon development environment…${C_RESET}"
  start_stack
  start_migrations
  start_control_plane
  start_frontend
  echo ""
  echo -e "${C_GREEN}${C_BOLD}Orchicon is running.${C_RESET}"
  echo -e "  Control plane:  ${C_DIM}http://localhost:8080${C_RESET}"
  echo -e "  Frontend:       ${C_DIM}http://localhost:5173${C_RESET}"
  echo -e "  SigNoz UI:      ${C_DIM}http://localhost:3301${C_RESET}"
  echo -e "  NATS monitor:   ${C_DIM}http://localhost:8222${C_RESET}"
  echo ""
  echo -e "  Logs:           ${C_DIM}tail -f $LOG_DIR/orchicon.log $LOG_DIR/vite.log${C_RESET}"
  echo -e "  Stop:           ${C_DIM}scripts/dev.sh stop${C_RESET}"
}

do_stop() {
  echo -e "${C_BOLD}Stopping Orchicon development environment…${C_RESET}"
  stop_pid "$PID_DIR/vite.pid" "frontend (Vite)"
  stop_pid "$PID_DIR/orchicon.pid" "control plane"
  log "stopping dev stack (Docker Compose)…"
  $COMPOSE down 2>/dev/null
  log_ok "dev stack stopped"
  echo ""
  echo -e "${C_YELLOW}Orchicon is stopped.${C_RESET}"
}

do_status() {
  echo -e "${C_BOLD}Orchicon development status${C_RESET}"
  echo ""

  # Control plane
  if is_running "$PID_DIR/orchicon.pid"; then
    local cp_pid; cp_pid="$(cat "$PID_DIR/orchicon.pid")"
    if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
      log_ok "control plane   ${C_DIM}running (pid $cp_pid) ${C_GREEN}healthy${C_RESET}"
    else
      log_warn "control plane   ${C_DIM}running (pid $cp_pid) ${C_YELLOW}not responding${C_RESET}"
    fi
  else
    log_err "control plane   ${C_DIM}stopped${C_RESET}"
  fi

  # Frontend
  if is_running "$PID_DIR/vite.pid"; then
    local fe_pid; fe_pid="$(cat "$PID_DIR/vite.pid")"
    if curl -sf http://localhost:5173/ >/dev/null 2>&1; then
      log_ok "frontend (Vite) ${C_DIM}running (pid $fe_pid) ${C_GREEN}serving${C_RESET}"
    else
      log_warn "frontend (Vite) ${C_DIM}running (pid $fe_pid) ${C_YELLOW}not responding${C_RESET}"
    fi
  else
    log_err "frontend (Vite) ${C_DIM}stopped${C_RESET}"
  fi

  # Docker Compose services
  echo ""
  echo -e "${C_BOLD}Docker Compose services:${C_RESET}"
  $COMPOSE ps --format "table {{.Name}}\t{{.Service}}\t{{.Status}}" 2>/dev/null || log_err "docker compose unavailable"

  # Quick connectivity check
  echo ""
  echo -e "${C_BOLD}Endpoints:${C_RESET}"
  local ep_url ep_label
  while IFS='|' read -r ep_url ep_label; do
    if curl -sf "$ep_url" >/dev/null 2>&1; then
      log_ok "$ep_label   ${C_DIM}$ep_url ${C_GREEN}ok${C_RESET}"
    else
      log_err "$ep_label   ${C_DIM}$ep_url ${C_RED}unreachable${C_RESET}"
    fi
  done <<'ENDPOINTS'
http://localhost:8080/healthz|Control plane
http://localhost:5173/|Frontend
http://localhost:8222/healthz|NATS
ENDPOINTS
}

do_logs() {
  log "tailing control plane + frontend logs (Ctrl+C to stop)…"
  tail -f "$LOG_DIR/orchicon.log" "$LOG_DIR/vite.log" 2>/dev/null || log_err "no logs found — is Orchicon running?"
}

do_restart() {
  do_stop
  echo ""
  sleep 1
  do_start
}

# --- Main -------------------------------------------------------------------
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
    echo "  start    Start dev stack + control plane + frontend"
    echo "  stop     Stop everything"
    echo "  status   Show status of all components"
    echo "  restart  Stop then start"
    echo "  logs     Tail control-plane + frontend logs"
    exit 1
    ;;
esac
