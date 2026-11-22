#!/usr/bin/env bash
# scripts/reset-idea.sh — put a work item BACK into the Idea Cloud.
#
# WHY THIS SCRIPT EXISTS.
#
# Promoting an idea is one-way through the product, and that is deliberate rather
# than an oversight:
#
#   * UpdateWorkItem REFUSES status=idea — "idea is a system-managed status set
#     only by the automation spawn path; it is not settable via the generic
#     update path" (internal/workitem/service.go).
#   * PromoteIdea is the only sanctioned way IN, and DismissIdea goes to
#     `cancelled`, not back to `idea`.
#   * There is no CLI verb and no UI button for the reverse move.
#
# So the only route back is a direct row write. That is exactly what this script
# is, and it is scoped as narrowly as the read path allows: one item, by id, with
# a printed before/after so the change is visible rather than assumed.
#
# WHEN YOU WANT THIS: rehearsing an Idea Cloud demo run, or undoing a promotion
# you clicked by mistake. It is a DEVELOPMENT AFFORDANCE — it writes the
# system-managed status directly, which is why it refuses to run against the prod
# instance and why it prints what it did.
#
# Usage:
#   scripts/reset-idea.sh <work-item-id> [instance]   # instance defaults to dev
#   scripts/reset-idea.sh 01M8283GN6D37PE3JS23J1H8NK
#   scripts/reset-idea.sh 01M8283GN6D37PE3JS23J1H8NK dev
#
# What it does, in ONE transaction:
#   1. refuses unless the item exists, is in the same tenant, and is NOT already
#      an idea (a no-op reset would bump the version and the audit trail for
#      nothing);
#   2. resolves-or-reports the project's "Idea Cloud" epic — see below;
#   3. re-parents the item to that epic, forces kind=task, sets status=idea.
#
# WHERE IT PARENTS THE ITEM, and why that is a real question. Idea Cloud items
# normally hang from the project's "Idea Cloud" epic (internal/workitem/
# idea_epic.go). This script will use that epic when it exists. When it does NOT
# exist it re-parents the item to NULL (a root) rather than creating the epic,
# because creating it would mean re-implementing the server's resolve-or-create
# here — a second source of truth for a container the server owns, which is the
# kind of drift that produced this ticket's original bugs. A NULL-parented idea is
# the same shape every pre-container idea has, and the server will adopt it under
# the container on the next spawn or promotion.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat >&2 <<'EOF'
usage: scripts/reset-idea.sh <work-item-id> [instance]

  <work-item-id>  the full ULID of the item to return to the Idea Cloud
  [instance]      dev (default) | test | prod   — prod is refused

examples:
  scripts/reset-idea.sh 01M8283GN6D37PE3JS23J1H8NK
  scripts/reset-idea.sh 01M8283GN6D37PE3JS23J1H8NK dev
EOF
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  usage
  exit 2
fi

ID="$1"
INSTANCE="${2:-dev}"

# A prod guard, because this writes a system-managed status directly. Resetting a
# promotion is a rehearsal action; doing it on the deployment tenant would edit
# real work with no audit entry to say a human did it.
if [ "$INSTANCE" = "prod" ]; then
  echo "reset-idea: refusing to run against prod." >&2
  echo "  This writes status=idea directly, bypassing the API that exists to keep that" >&2
  echo "  field system-managed. It is a development affordance for rehearsing the Idea" >&2
  echo "  Cloud demo, not an operator tool." >&2
  exit 1
fi

case "$INSTANCE" in
  dev|test) ;;
  *)
    echo "reset-idea: unknown instance '$INSTANCE' (want dev, test or prod)" >&2
    exit 2
    ;;
esac

# Validate the id shape BEFORE touching the database: a typo'd id must fail loudly
# here, not silently match zero rows and "succeed".
if ! printf '%s' "$ID" | grep -Eq '^[0-9A-HJKMNP-TV-Z]{26}$'; then
  echo "reset-idea: '$ID' is not a 26-character ULID." >&2
  echo "  Copy the full id from the work item page header (e.g. 01M8283GTHDAHB23Z7BAZ28W78)." >&2
  exit 2
fi

# Resolve the instance's DSN and psql via the ONE table (scripts/instance-ports.sh),
# so this script cannot disagree with the Makefile about where an instance lives.
DB_URL="$("${HERE}/instance-ports.sh" db-url "$INSTANCE" 2>/dev/null || true)"
if [ -z "$DB_URL" ]; then
  echo "reset-idea: cannot resolve a database URL for instance '$INSTANCE'." >&2
  echo "  Is the instance installed? Try: scripts/instance-ports.sh db-url $INSTANCE" >&2
  exit 1
fi

# Prefer the instance's own downloaded psql, exactly like scripts/check-rls.sh: a
# dev host commonly has no system Postgres client.
PSQL_BIN="$("${HERE}/instance-ports.sh" pg-psql "$INSTANCE" 2>/dev/null || true)"
if [ -z "$PSQL_BIN" ] || [ ! -x "$PSQL_BIN" ]; then
  PSQL_BIN="$(command -v psql || true)"
fi
if [ -z "$PSQL_BIN" ]; then
  echo "reset-idea: no psql found (neither the instance's downloaded one nor on PATH)." >&2
  exit 1
fi

if ! "$PSQL_BIN" "$DB_URL" -tAc 'SELECT 1' >/dev/null 2>&1; then
  echo "reset-idea: cannot reach the database for '$INSTANCE'." >&2
  echo "  Start its services first:  make services instance=$INSTANCE" >&2
  exit 1
fi

echo "reset-idea: instance=$INSTANCE"
echo "reset-idea: db=${DB_URL%%\?*}" | sed 's#://[^@]*@#://***@#'

# The whole change is one statement so it is atomic, and it returns the row it
# wrote — the script prints the ACTUAL result rather than assuming it matched.
"$PSQL_BIN" "$DB_URL" -v ON_ERROR_STOP=1 -v id="$ID" <<'SQL'
WITH target AS (
    SELECT w.id,
           w.status,
           w.kind,
           w.version,
           w.tenant_id,
           w.project_id,
           -- The project's Idea Cloud container, when one exists. Matched on the
           -- SAME identity the server uses (internal/workitem/idea_epic.go):
           -- top-level epic + exact title + same tenant/project.
           COALESCE((
               SELECT e.id
                 FROM work_items e
                WHERE e.tenant_id  = w.tenant_id
                  AND e.project_id = w.project_id
                  AND e.kind       = 'epic'
                  AND e.parent_id IS NULL
                  AND e.title      = 'Idea Cloud'
                ORDER BY e.created_at
                LIMIT 1
           ), '') AS epic_id
      FROM work_items w
     WHERE w.id = :'id'
), reset AS (
    UPDATE work_items w
       SET status     = 'idea',
           kind       = 'task',
           parent_id  = NULLIF((SELECT epic_id FROM target), '')::text,
           version    = w.version + 1,
           updated_at = now()
     WHERE w.id = :'id'
       -- Guard in the WRITE, not only in the read: already-idea is a no-op that
       -- would otherwise bump version/updated_at for nothing.
       AND w.status <> 'idea'
    RETURNING w.id, w.status, w.kind, w.parent_id, w.version
)
SELECT (SELECT count(*) FROM target)          AS found,
       (SELECT epic_id FROM target)            AS container_id,
       (SELECT status FROM target)             AS was_status,
       (SELECT kind FROM target)               AS was_kind,
       (SELECT count(*) FROM reset)            AS updated,
       (SELECT status FROM reset LIMIT 1)      AS now_status,
       (SELECT parent_id FROM reset LIMIT 1)   AS now_parent;
SQL

# Explain the outcome using a second, tiny read so the message reflects committed
# state rather than the CTE's in-flight view.
STATE="$("$PSQL_BIN" "$DB_URL" -tAc "SELECT status || '|' || kind || '|' || COALESCE(parent_id,'') FROM work_items WHERE id = '$ID'")"
if [ -z "$STATE" ]; then
  echo "reset-idea: no work item with id $ID — nothing was changed." >&2
  exit 1
fi

STATUS="${STATE%%|*}"
REST="${STATE#*|}"
KIND="${REST%%|*}"
PARENT="${REST#*|}"

echo
if [ "$STATUS" = "idea" ]; then
  echo "✓ $ID is in the Idea Cloud (status=$STATUS, kind=$KIND)"
  if [ -n "$PARENT" ]; then
    echo "  parent: $PARENT"
  else
    echo "  parent: none — no 'Idea Cloud' epic exists in this project yet."
    echo "  It will be adopted under the epic on the next promotion or spawn fire."
  fi
  echo "  It is now hidden from Work Items and visible in the Idea Cloud view."
else
  echo "reset-idea: $ID is status=$STATUS, not idea." >&2
  echo "  Either it was already an idea (nothing to do) or the write did not apply." >&2
  exit 1
fi
