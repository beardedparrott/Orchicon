-- Reverse of 20260919000000_always_container_runtime (best-effort).
--
-- The forward migration added projects.default_runtime_image +
-- projects.execution_mode (with its CHECK). This drops the constraint
-- then the columns. Per-row default values are lost on rollback; the
-- down migration is a dev-rollback convenience, not a data guarantee;
-- migrations are forward-only and this file exists to satisfy the
-- paired _down.sql convention.
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_execution_mode_check;
ALTER TABLE projects DROP COLUMN IF EXISTS default_runtime_image;
ALTER TABLE projects DROP COLUMN IF EXISTS execution_mode;
