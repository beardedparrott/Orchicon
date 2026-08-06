-- Down migration for 20260806000000_normalize_work_item_kind.sql

-- The CHECK constraint is a hardening guard; dropping it restores the
-- pre-fix unconstrained text column. The data repair itself is not
-- reversed (it only folded casing to canonical values, so reverting it
-- would reintroduce the bug).
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_kind_check;
