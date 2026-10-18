#!/usr/bin/env bash
# scripts/instance-ports.sh — the per-instance PORTS and DSN for the inner loop.
#
# WHY THIS EXISTS AS ITS OWN SCRIPT. `scripts/container.sh` used to be the only
# place the shell side resolved an instance's ports, and the Makefile needs two of
# them AT PARSE TIME (DB_URL at `Makefile:DB_URL`, and the plane's HTTP/NATS
# endpoints in the `run` recipe) — before any Go binary exists, so this cannot be
# a Go call. The container launcher is gone (there is no instance container and no
# image: the plane and its services are host processes), but the TABLE it read is
# not: it lives in scripts/lib/instances.sh instance_info(), shared with the
# teardown and pinned to the Go half by TestInstallPortDefaultsMatchShellInstanceTable.
# So this is that table's query surface and nothing else — no engine, no docker, no
# side effects, three pure verbs.
#
#   scripts/instance-ports.sh services-port <inst> <postgres|nats|nats-mon|control|grafana>
#   scripts/instance-ports.sh db-url <inst>
#   scripts/instance-ports.sh pg-psql <inst>
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/instances.sh
. "${HERE}/lib/instances.sh"

# _recorded_port prints the port an instance's INSTALL RECORDED for one env key
# (ports.json in the instance state dir), or nothing when there is no usable
# record.
#
# WHY THE RECORD IS CONSULTED FIRST: `orchicon services up` (the Go half,
# cmd/orchicon/services.go servicesPortsFor) binds the RECORDED ports when the
# instance has them, because re-running over a live stack must keep the ports it
# is already on. The DSN the inner loop derives must therefore agree with the
# RECORD, not the defaults — otherwise `make run` would point the plane at a port
# the started services never bound (exactly the "a URL pointing at a port this
# install never bound" defect the per-instance table exists to prevent).
#
# THE RECORD IS ALL-OR-NOTHING, MATCHING THE GO HALF EXACTLY: servicesPortsFor
# only trusts the record when it carries BOTH postgres and nats ("> 0"), and
# falls back to defaults + overrides otherwise. A partial record must NOT be
# consulted key-by-key here or the two halves would disagree on the instance shape
# they resolve — the very drift this derivation exists to prevent.
_recorded_port() {
  local inst="$1" env="$2" file pg nats
  instance_info "$inst" || return 1
  file="$HOST_DATA_DIR/ports.json"
  [ -f "$file" ] || return 0
  pg="$(sed -n 's/.*"ORCHICON_POSTGRES_PORT"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$file" | head -1)"
  nats="$(sed -n 's/.*"ORCHICON_NATS_PORT"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$file" | head -1)"
  [ -n "$pg" ] && [ -n "$nats" ] || return 0
  sed -n "s/.*\"${env}\"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p" "$file" | head -1
}

# services_port prints one service's HOST port for an instance.
#
# The Go side (cmd/orchicon/install_ports.go) is the other half of this table and
# a test asserts the two agree; both apply the same ORCHICON_*_PORT knobs at
# instance_info's ONE override choke point. The RECORD is consulted FIRST (see
# _recorded_port), matching the Go half's record-first rule; the defaults+env path
# below is the developer-who-never-installed shape.
services_port() {
  local inst="${1:-dev}" svc="${2:-postgres}" key="" def=""
  instance_info "$inst" || return 1
  case "$svc" in
    postgres|pg) key=ORCHICON_POSTGRES_PORT; def="$PG_PORT" ;;
    nats) key=ORCHICON_NATS_PORT; def="$NATS_PORT" ;;
    nats-mon|nats-monitor) key=ORCHICON_NATS_MONITOR_PORT; def="$NATS_MON_PORT" ;;
    control|plane) key=ORCHICON_CONTROL_PORT; def="$PLANE_HTTP_PORT" ;;
    grafana) key=ORCHICON_GRAFANA_PORT; def="$GRAFANA_HOST_PORT" ;;
    *) echo "unknown service: $svc (use postgres|nats|nats-mon|control|grafana)" >&2; return 1 ;;
  esac
  local rec
  rec="$(_recorded_port "$inst" "$key")"
  if [ -n "$rec" ]; then
    echo "$rec"
  else
    echo "$def"
  fi
}

# db_url prints the Postgres DSN for an instance, DERIVED from services_port so
# the DSN and the bound port can never disagree. This is what the Makefile's
# DB_URL resolves through, so `make migrate instance=prod` reaches prod's 5433.
db_url() {
  local inst="${1:-dev}" port
  port="$(services_port "$inst" postgres)" || return 1
  echo "postgres://orchicon:orchicon@localhost:${port}/orchicon?sslmode=disable"
}

# pg_psql prints the psql of the PostgreSQL this instance RESOLVED, read from the
# record the install wrote (<hostDataDir>/infra-binaries.json), or nothing when
# there is no such record.
#
# IT IS THE RUNG THAT MAKES check-rls.sh REACHABLE ON A HOST WITH NO SYSTEM PSQL:
# a developer with no system psql still has one here, at
# <DataDir>/bin/postgres/bin/psql, which is not on PATH. The record is written
# with a stable 2-space indent (cmd/orchicon/binaries.go recordInfraBinaries), so
# the "psql" key is read without a jq dependency.
pg_psql() {
  local inst="${1:-dev}" file
  instance_info "$inst" || return 1
  file="$HOST_DATA_DIR/infra-binaries.json"
  [ -f "$file" ] || return 1
  sed -n 's/.*"psql"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$file" | head -1
}

usage() {
  cat <<'EOF'
usage: scripts/instance-ports.sh <verb> [instance]

verbs:
  services-port <inst> <postgres|nats|nats-mon|control|grafana>
                                   ONE service's PORT for an instance
  db-url <inst>                    the instance's Postgres DSN
  pg-psql <inst>                   the psql binary the instance's install recorded

Instances: dev (default) | prod | test. Pure queries: no engine, no side effects.
EOF
}

verb="${1:-}"
[ "$#" -gt 0 ] && shift
case "$verb" in
  services-port|service-port) services_port "${1:-dev}" "${2:-postgres}" ;;
  db-url)                     db_url "${1:-dev}" ;;
  pg-psql)                    pg_psql "${1:-dev}" ;;
  help|-h|--help)             usage ;;
  "")                         usage >&2; exit 2 ;;
  *)                          echo "unknown verb: $verb" >&2; usage >&2; exit 2 ;;
esac
