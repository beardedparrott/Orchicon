-- Make project_dir non-nullable with empty string default.
-- Existing NULL values are backfilled to empty string so existing
-- projects remain loadable without code changes.

UPDATE "projects" SET "project_dir" = '' WHERE "project_dir" IS NULL;

ALTER TABLE "projects"
  ALTER COLUMN "project_dir" SET DEFAULT '',
  ALTER COLUMN "project_dir" SET NOT NULL;

COMMENT ON COLUMN "projects"."project_dir" IS 'Root directory of the project on the local filesystem (docs/02 §2.1). Empty string when unset.';
