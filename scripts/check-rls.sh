#!/usr/bin/env bash
# RLS CI gate (docs/09_Database_Schema.md §8, §8.5).
#
# Fails if any table with a tenant_id column lacks the tenant_isolation
# RLS policy. This is the migration-time check that prevents silent
# holes where a new tenant-scoped table ships without the backstop.
#
# Usage: check-rls.sh <postgres-url>
#   e.g. scripts/check-rls.sh "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"
#
# If psql is not on PATH (common on dev hosts) or the URL is unreachable (the
# instance's services are not up), falls back to the instance's OWN downloaded psql
# (`scripts/instance-ports.sh pg-psql`, from infra-binaries.json). The downloaded
# rung is what makes this gate reachable on a host with no container engine and no
# system Postgres — the de-containerized inner loop.
#
# When NO database is reachable at all (host psql absent/unreachable AND no
# downloaded psql for the instance) the gate is SKIPPED with a warning and
# exits 0 — so `make rebuild-dev`/`make ci` does not hard-fail on a dev box
# where the instance isn't up yet. CI (which always has a reachable DB) still
# enforces the gate.
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <postgres-url>" >&2
  exit 2
fi

URL="$1"
# This script lives in scripts/, so the repo root is one level up. Used to reach
# scripts/instance-ports.sh, which resolves the instance's DOWNLOADED psql.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The instance whose downloaded Postgres holds the DB. The Makefile resolves
# DB_URL per instance and passes only the URL, so the instance name comes from
# ORCHICON_INSTANCE (the same variable the plane and the launcher read).
INSTANCE="${ORCHICON_INSTANCE:-dev}"

# the_bindir_psql prints the psql of the PostgreSQL this instance RESOLVED, via
# scripts/instance-ports.sh pg-psql (which reads <hostDataDir>/infra-binaries.json),
# or nothing when there is no such record. ORCHICON_PG_PSQL wins outright.
#
# THIS IS THE RUNG THAT KEEPS THE GATE REACHABLE ON A HOST WITH NO SYSTEM PSQL: a
# developer with no system psql still has one, under the instance's own data dir,
# and it is not on PATH. Without it the gate SKIPS on exactly the host shape this
# product ships.
the_bindir_psql() {
  if [ -n "${ORCHICON_PG_PSQL:-}" ]; then
    printf '%s\n' "$ORCHICON_PG_PSQL"
    return 0
  fi
  local sh="${REPO_ROOT}/scripts/instance-ports.sh"
  [ -x "$sh" ] || return 0
  "$sh" pg-psql "$INSTANCE" 2>/dev/null || true
}

# Stream SQL results when a DB is reachable (host psql, then the instance's own
# downloaded psql). Returns 3 when no DB is reachable so the caller can skip the gate.
run_sql() {
  if command -v psql >/dev/null 2>&1 && psql "$URL" -c "SELECT 1" >/dev/null 2>&1; then
    psql "$URL" -t -A -F '|'
    return 0
  fi
  local bindir_psql
  bindir_psql="$(the_bindir_psql)"
  if [ -n "$bindir_psql" ] && [ -x "$bindir_psql" ] && "$bindir_psql" "$URL" -c "SELECT 1" >/dev/null 2>&1; then
    "$bindir_psql" "$URL" -t -A -F '|'
    return 0
  fi
  return 3
}

# Tables that have a tenant_id column but must NOT carry the RLS
# backstop (none expected — the tenants table itself has no tenant_id).
# Extend this allowlist only with a documented exception.
ALLOWLIST_REGEX='^$'

set +e
violations=$(run_sql <<SQL
SELECT c.table_name
FROM information_schema.columns c
JOIN pg_tables t ON t.tablename = c.table_name AND t.schemaname = c.table_schema
WHERE c.column_name = 'tenant_id'
  AND c.table_schema = 'public'
  AND NOT EXISTS (
    SELECT 1 FROM pg_policy p
    JOIN pg_class cls ON cls.oid = p.polrelid
    JOIN pg_namespace n ON n.oid = cls.relnamespace
    WHERE cls.relname = c.table_name
      AND n.nspname = 'public'
      AND p.polname = 'tenant_isolation'
  )
  AND c.table_name !~ '$ALLOWLIST_REGEX'
ORDER BY c.table_name;
SQL
)
rc=$?
set -e

if [ "$rc" -eq 3 ]; then
  echo "RLS gate SKIPPED: no reachable Postgres (no host psql, and no downloaded psql for '$INSTANCE')." >&2
  echo "Start the instance's services ('make services instance=dev', or scripts/orchicon.sh start dev) or run with a reachable DB_URL to enforce this gate." >&2
  exit 0
fi
if [ "$rc" -ne 0 ]; then
  echo "RLS gate ERROR: could not run the RLS query (exit $rc)" >&2
  exit "$rc"
fi

if [ -n "$violations" ]; then
  echo "RLS gate FAILED: tenant_id tables missing tenant_isolation policy:" >&2
  echo "$violations" >&2
  exit 1
fi

echo "RLS gate OK: all tenant_id tables have the tenant_isolation policy."
