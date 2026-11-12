-- Reverse of 20261112000000_runtime_reaped_at.sql.
--
-- Drops the reap marker. No WORK is lost either way: the marker records only that the platform already tried to
-- reap a terminal run's container, and dropping it makes the sweep treat old runs as candidates again — bounded
-- and harmless (a reap of a gone container is a no-op), just noisier, which is what the column was added to fix.
DROP INDEX IF EXISTS workflow_runs_runtime_reap_idx;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS runtime_reaped_at;
