-- Reverse of 20260922000000_file_edit_ledger (best-effort).
--
-- The forward migration created file_edit_ledger. This drops the table.
-- Ledger rows are derived telemetry (recomputable from file state); the
-- down migration is a dev-rollback convenience, not a data guarantee;
-- migrations are forward-only and this file exists to satisfy the paired
-- _down.sql convention.
DROP TABLE IF EXISTS file_edit_ledger;
