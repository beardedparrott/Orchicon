-- Work-item execution_mode override.
--
-- NULL is the ONLY "inherit from the project" representation: an empty string
-- would be a third state no reader could tell from a typo. Note the asymmetry
-- with projects.execution_mode, which is NOT NULL DEFAULT 'runtime' — a project
-- is the ROOT of the chain (nothing above it but the tenant default), while a
-- work item sits under a project it can inherit from.
--
-- The resolution chain (item -> project -> tenant default) is implemented ONCE,
-- in db.EffectiveExecutionMode (internal/db/execution_mode.go). Nothing else may
-- re-derive it: three duplicated project-only helpers were collapsed onto that
-- resolver in the same change.
--
-- The CHECK mirrors the projects precedent (20260919000000_always_container_runtime.sql)
-- and is declared here rather than in db/schema.hcl because this repo's
-- schema.hcl carries NO check constraints at all — projects_execution_mode_check
-- lives only in its migration too. Keeping that shape means the next
-- `atlas migrate diff` behaves exactly as it did before this change.
--
-- Forward-only: no backfill. Every existing row is legitimately "inherit".
ALTER TABLE work_items
	ADD COLUMN execution_mode TEXT NULL;
ALTER TABLE work_items
	ADD CONSTRAINT work_items_execution_mode_check
	CHECK (execution_mode IS NULL OR execution_mode IN ('runtime', 'local'));
