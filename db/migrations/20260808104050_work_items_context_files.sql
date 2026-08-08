-- Work item context: absolute file/directory paths provided as worker
-- context, mirroring projects.context_files (the work item may attach its
-- own context "in exactly the same way as projects").
--
-- Stored as a JSON array of absolute paths (files OR directories). When a
-- worker is dispatched for this item, the composite prompt renders this
-- list (see internal/contextfiles) alongside the project's context files.
-- Same table -> no new RLS policy needed (row-level RLS already exists).
ALTER TABLE "work_items" ADD COLUMN IF NOT EXISTS "context_files" jsonb NULL;
COMMENT ON COLUMN "work_items"."context_files" IS 'Absolute file/directory paths provided as worker context (same model as projects.context_files).';
