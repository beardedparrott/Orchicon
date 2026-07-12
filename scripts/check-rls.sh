#!/usr/bin/env bash
# RLS CI gate (docs/09_Database_Schema.md §8, §8.5).
#
# Fails if any table with a tenant_id column lacks the tenant_isolation
# RLS policy. This is the migration-time check that prevents silent
# holes where a new tenant-scoped table ships without the backstop.
#
# Usage: check-rls.sh <postgres-url>
#   e.g. scripts/check-rls.sh "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <postgres-url>" >&2
  exit 2
fi

URL="$1"

# Tables that have a tenant_id column but must NOT carry the RLS
# backstop (none expected — the tenants table itself has no tenant_id).
# Extend this allowlist only with a documented exception.
ALLOWLIST_REGEX='^$'

violations=$(psql "$URL" -t -A -F '|' <<'SQL'
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

if [ -n "$violations" ]; then
  echo "RLS gate FAILED: tenant_id tables missing tenant_isolation policy:" >&2
  echo "$violations" >&2
  exit 1
fi

echo "RLS gate OK: all tenant_id tables have the tenant_isolation policy."
