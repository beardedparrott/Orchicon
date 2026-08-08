-- Down migration for 20260808120000_work_items_acceptance_review.sql
--
-- The column has a default of '' and holds no schema constraints beyond
-- the column itself, so dropping it fully reverses the migration. Data
-- loss is acceptable for a down migration (forward-only in practice).
ALTER TABLE work_items DROP COLUMN IF EXISTS acceptance_review;
