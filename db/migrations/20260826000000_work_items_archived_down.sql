-- Reverse 20260826000000_work_items_archived.sql (rollback between binary
-- versions). Drops the archive columns and their index.
DROP INDEX IF EXISTS idx_work_items_archived_at;
ALTER TABLE "work_items" DROP COLUMN IF EXISTS "archived_at";
ALTER TABLE "work_items" DROP COLUMN IF EXISTS "archived_from_status";
