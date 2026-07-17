-- Add project_dir and context_files columns to projects table.
--
-- project_dir: the root directory of the project on the local filesystem.
--   Workers will read files from this directory when they need context.
-- context_files: a JSON array of relative file paths (from project_dir)
--   that the user has selected as context for workers. When a workflow
--   dispatches a worker for this project, the contents of these files
--   are injected into the composite prompt.

ALTER TABLE "projects"
  ADD COLUMN "project_dir" text,
  ADD COLUMN "context_files" jsonb NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN "projects"."project_dir" IS 'Root directory of the project on the local filesystem (docs/02 §2.1).';
COMMENT ON COLUMN "projects"."context_files" IS 'Relative file paths selected as context for workers (docs/02 §2.1).';
