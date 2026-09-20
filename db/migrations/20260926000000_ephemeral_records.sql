-- The ephemeral marker: machine-managed transient records (Ask Orchicon Quick Work).
--
-- Quick Work creates a worker, a workflow and a work item per job, fires the run,
-- then removes them. The operator's requirement: "hard-delete the work item once
-- complete (no invisible records)". While a record lives it must be invisible to
-- humans, which is what this column buys; when the job ends the row is
-- HARD-DELETED, so there is no "deleted but still here" state to leak.
--
--   work_items.ephemeral — the item carrying the job
--   workers.ephemeral    — the throwaway worker pinned to the Quick Work agent's own model_ref
--   workflows.ephemeral  — the throwaway workflow the job runs
--
-- Default false, so every existing row and every ordinary create keeps its current
-- behaviour. The read gate (internal/db) defaults to EXCLUDING ephemeral rows, so
-- this migration needs no backfill and changes no view until something sets the
-- flag — nothing to repair, nothing to re-render.
--
-- Additive-only (AGENTS.md invariant #9): one NOT NULL column with a default per
-- table, no destructive DDL. Follows the _down.sql pairing convention of every
-- other migration here (which is the convention internal/migrate implements).
ALTER TABLE "work_items"
  ADD COLUMN IF NOT EXISTS "ephemeral" BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE "workers"
  ADD COLUMN IF NOT EXISTS "ephemeral" BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE "workflows"
  ADD COLUMN IF NOT EXISTS "ephemeral" BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN work_items.ephemeral
  IS 'Machine-managed transient item (Quick Work): hidden from every human work-item view and hard-deleted when its job ends. Never a parent, never recurring.';
COMMENT ON COLUMN workers.ephemeral
  IS 'Machine-managed transient worker (Quick Work): hidden from the Workers view and hard-deleted when its job ends.';
COMMENT ON COLUMN workflows.ephemeral
  IS 'Machine-managed transient workflow (Quick Work): hidden from the Workflows view and hard-deleted when its job ends.';

-- PARTIAL INDEXES for the abandonment sweep.
--
-- A crashed agent leaves an ephemeral record behind: invisible, and still there —
-- the exact "invisible record" the feature exists to avoid, arriving by a
-- different route. A periodic sweep deletes ephemeral rows older than a window,
-- and it runs on a timer against tables (work_items especially) that can hold
-- millions of ordinary rows. Indexing only the ephemeral partition keeps that
-- scan proportional to the handful of live transients rather than to the table,
-- and costs almost nothing to maintain because the indexed predicate matches
-- almost no rows.
CREATE INDEX IF NOT EXISTS "idx_work_items_ephemeral_created"
  ON "work_items" (created_at) WHERE ephemeral;

CREATE INDEX IF NOT EXISTS "idx_workers_ephemeral_created"
  ON "workers" (created_at) WHERE ephemeral;

CREATE INDEX IF NOT EXISTS "idx_workflows_ephemeral_created"
  ON "workflows" (created_at) WHERE ephemeral;
