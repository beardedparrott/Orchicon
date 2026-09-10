#!/usr/bin/env bash
# One-off cleanup of the historical published-outbox backlog (the 8.4M-row /
# 8.4 GB table). NOT a boot migration: an 8M-row DELETE inside migrate.Run would
# block plane boot on WAL, locks and autovacuum. Run it by hand, off-peak.
#
#   scripts/outbox-backlog-cleanup.sh [DSN] [RETENTION_DAYS]
#
# DSN defaults to $ORCHICON_POSTGRES_DSN; RETENTION_DAYS defaults to 7 (the same
# window the scheduled retention pass uses, ORCHICON_OUTBOX_RETENTION_DAYS).
#
# Behaviour: deletes PUBLISHED rows older than the window, oldest first, in
# bounded batches (ROW_BATCH rows per statement, ~SLEEP_MS between batches) so
# the lock window and WAL spike per statement stay small. Unpublished rows are
# never touched. Stops when a batch comes back empty or the MAX_MINUTES budget
# is exhausted (re-runnable: just run it again).
#
# After it finishes, vacuum:
#   VACUUM (ANALYZE) outbox;                      -- online, plain, no FULL
# Optionally, in a maintenance window, if bloat is still bad:
#   VACUUM FULL outbox;                           -- takes an ACCESS EXCLUSIVE lock
#   -- or pg_repack, which avoids the long lock
# And only if index bloat is measurable:
#   REINDEX INDEX CONCURRENTLY outbox_unpublished_idx;
set -euo pipefail

DSN="${1:-${ORCHICON_POSTGRES_DSN:-}}"
RETENTION_DAYS="${2:-7}"
ROW_BATCH="${ROW_BATCH:-10000}"
SLEEP_MS="${SLEEP_MS:-200}"
MAX_MINUTES="${MAX_MINUTES:-120}"

if [[ -z "$DSN" ]]; then
  echo "error: no DSN. Pass it as \$1 or set ORCHICON_POSTGRES_DSN." >&2
  exit 2
fi
if ! [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]] || [[ "$RETENTION_DAYS" -lt 1 ]]; then
  echo "error: RETENTION_DAYS must be an integer >= 1 (got '$RETENTION_DAYS')" >&2
  exit 2
fi

echo "outbox backlog cleanup"
echo "  retention window : ${RETENTION_DAYS} days"
echo "  rows per batch   : ${ROW_BATCH}"
echo "  sleep between    : ${SLEEP_MS} ms"
echo "  budget           : ${MAX_MINUTES} min"
echo

started=$(date +%s)
total=0
batch=0
while :; do
  batch=$((batch + 1))
  # DELETE ... RETURNING id so each batch reports exactly how many rows it
  # removed (psql -tA prints one id per line; empty output = nothing eligible).
  deleted=$(psql "$DSN" -v ON_ERROR_STOP=1 -tA <<SQL
WITH victims AS (
  SELECT id FROM outbox
  WHERE published_at IS NOT NULL
    AND published_at < now() - interval '${RETENTION_DAYS} days'
  ORDER BY published_at
  LIMIT ${ROW_BATCH}
)
DELETE FROM outbox WHERE id IN (SELECT id FROM victims) RETURNING id;
SQL
)
  n=$(printf '%s\n' "$deleted" | grep -c . || true)
  total=$((total + n))
  elapsed=$(( $(date +%s) - started ))
  echo "batch ${batch}: deleted ${n} rows (total ${total}, ${elapsed}s elapsed)"

  if [[ "$n" -eq 0 ]]; then
    echo "no eligible published rows left."
    break
  fi
  if [[ "$elapsed" -ge $((MAX_MINUTES * 60)) ]]; then
    echo "budget of ${MAX_MINUTES} min reached — re-run to continue." >&2
    break
  fi
  sleep "$(awk -v ms="$SLEEP_MS" 'BEGIN { printf "%.3f", ms/1000 }')"
done

cat <<'EOM'

cleanup complete. Now reclaim space:

  VACUUM (ANALYZE) outbox;    -- online; plain VACUUM, no FULL, safe on a live plane

Only if bloat is still significant after that (maintenance window):

  VACUUM FULL outbox;         -- ACCESS EXCLUSIVE lock for the duration
  -- or: pg_repack -t outbox  -- rewrites without a long exclusive lock

Only if index bloat is measurable:

  REINDEX INDEX CONCURRENTLY outbox_unpublished_idx;

Steady-state retention after this one-off is handled by the relay's scheduled
prune (ORCHICON_OUTBOX_RETENTION_DAYS / ORCHICON_OUTBOX_PRUNE_BATCH /
ORCHICON_OUTBOX_PRUNE_INTERVAL — see docs/outbox-retention.md).
EOM
