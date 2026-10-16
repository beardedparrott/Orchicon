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
# If psql is not on PATH (common on dev hosts) or the URL is unreachable
# (postgres runs inside the single-container instance, not on the host),
# falls back to the psql in the `orchicon services` bindir and then to
# `docker exec` into the dev container instance.
#
# When NO database is reachable at all (host psql absent/unreachable AND the
# instance container is not running) the gate is SKIPPED with a warning and
# exits 0 — so `make rebuild-dev`/`make ci` does not hard-fail on a dev box
# where the instance isn't up yet. CI (which always has a reachable DB, and
# runs after `container up`) still enforces the gate.
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <postgres-url>" >&2
  exit 2
fi

URL="$1"
CONTAINER="orchicon-cnt-dev"

# Stream SQL results when a DB is reachable (host psql first, then the
# `orchicon services` bindir, then the dev instance container). Returns 3 when no
# DB is reachable so the caller can skip the gate.
#
# THE BINDIR RUNG IS WHAT KEEPS THIS GATE ALIVE WITHOUT A CONTAINER. A developer
# running the host services has a perfectly good psql sitting in the acquired
# Postgres bindir and NOT on PATH — so without this rung the gate would report
# "no reachable Postgres" on a host whose Postgres was up and reachable. It is the
# same bindir `orchicon services` uses, resolved by the same function
# (ORCHICON_PG_BINDIR wins, else the instance's data dir).
run_sql() {
  if command -v psql >/dev/null 2>&1 && psql "$URL" -c "SELECT 1" >/dev/null 2>&1; then
    psql "$URL" -t -A -F '|'
    return 0
  fi
  local bindir_psql="${ORCHICON_PG_BINDIR:-$HOME/.local/share/orchicon-${ORCHICON_INSTANCE:-dev}/services/pg/bin}/psql"
  if [ -x "$bindir_psql" ] && "$bindir_psql" "$URL" -c "SELECT 1" >/dev/null 2>&1; then
    "$bindir_psql" "$URL" -t -A -F '|'
    return 0
  fi
  if docker exec -i "$CONTAINER" psql "$URL" -c "SELECT 1" >/dev/null 2>&1; then
    docker exec -i "$CONTAINER" psql "$URL" -t -A -F '|'
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
  echo "RLS gate SKIPPED: no reachable Postgres (host psql unavailable, no host-services bindir, and $CONTAINER not running)." >&2
  echo "Start the instance (scripts/container.sh up dev) or run with a reachable DB_URL to enforce this gate." >&2
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
