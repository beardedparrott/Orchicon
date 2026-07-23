-- Add structured prompt fields to worker_versions.
-- See docs/05_Worker_Specification.md §5.

ALTER TABLE worker_versions
  ADD COLUMN IF NOT EXISTS role       text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS skills     text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS behavior   text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS agents_md  text NOT NULL DEFAULT '';
