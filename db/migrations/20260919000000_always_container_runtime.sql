-- Always-container runtime: project-level default image + execution mode.
--
-- D1: the projects row owns both defaults. default_runtime_image NULL =
-- inherit tenant/base (empty work-item image resolves down the chain);
-- execution_mode runtime|local, default 'runtime' preserves current
-- opencode behavior. Forward-only; the paired _down.sql exists solely
-- for the dev-rollback convention.
ALTER TABLE projects
	ADD COLUMN default_runtime_image TEXT NULL,
	ADD COLUMN execution_mode TEXT NOT NULL DEFAULT 'runtime';
ALTER TABLE projects
	ADD CONSTRAINT projects_execution_mode_check CHECK (execution_mode IN ('runtime', 'local'));
