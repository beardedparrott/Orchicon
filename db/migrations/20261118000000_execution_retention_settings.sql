-- BOUNDED EXECUTION HISTORY: a scheduled prune an operator can actually configure.
--
-- THE PROBLEM THIS FIXES. `orchicon prune-executions` already existed and worked correctly, but it was
-- MANUAL-ONLY: nothing scheduled it, no reconciler called it, and no setting could describe it. So a
-- long-lived deployment accumulated execution history forever. Measured on a real deployment: the
-- `execution_session_parts` table reached 3154 MB holding 30474 rows, of which pgstattuple reported
-- 98.3% DEAD SPACE, and the `outbox` table reached 2102 MB of index bloat around a SINGLE row. Deleting
-- rows does not return disk to the OS — PostgreSQL marks the space reusable inside the relation — so
-- every operator who ever ran the prune still watched their data directory grow.
--
-- THREE STATES, DELIBERATELY DISTINCT, and this is the whole reason for separate columns rather than one
-- sentinel value:
--
--   * enabled = false            — "do not prune". What a never-configured tenant has, so the feature
--                                  ships OFF and cannot surprise an existing deployment.
--   * enabled = true, keep = 0   — a REAL and MEANINGFUL setting: discard all terminal execution history.
--   * enabled = true, keep = N   — keep the newest N executions, drop the rest.
--
-- `keep = 0` had to remain expressible, which a single `retention_keep` column defaulting to 0 could not
-- do: 0 would then be indistinguishable from "unset", the exact confusion the MaxConcurrentRunsSet field
-- in internal/db/settings.go already exists to work around. A boolean makes the two questions separate.
--
-- NULL, NOT 0, FOR THE KEEP COUNT: `keep` is nullable so "unset" is a state PostgreSQL can report
-- honestly, and the Go layer COALESCEs it to the built-in default of 150. That default is not arbitrary —
-- it is the value the prune CLI has always used.
ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS execution_retention_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS execution_retention_keep bigint,
  -- VACUUM is what actually returns the disk. Without it the prune only makes space REUSABLE, so the
  -- data directory keeps growing while the row counts fall — the failure that prompted this work.
  ADD COLUMN IF NOT EXISTS execution_retention_vacuum boolean NOT NULL DEFAULT true,
  -- Wall-clock ceiling for one pass. A prune that runs long enough to matter is a prune holding locks
  -- the control plane needs, so it gets a budget rather than an open-ended lock window.
  ADD COLUMN IF NOT EXISTS execution_retention_timeout_seconds integer NOT NULL DEFAULT 300,
  -- On by default with a conservative window: the outbox relay prunes on its own ticker, but only when
  -- ORCHICON_OUTBOX_RETENTION_DAYS is reachable — a tenant running defaults should still get bounded
  -- growth. 0 here means "inherit the env-var/code default", matching every other retention column in
  -- this table.
  ADD COLUMN IF NOT EXISTS outbox_retention_days integer NOT NULL DEFAULT 0;
