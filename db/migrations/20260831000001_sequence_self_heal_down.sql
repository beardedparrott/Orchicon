ALTER TABLE work_items DROP COLUMN IF EXISTS sequence_attempts;
ALTER TABLE work_items DROP COLUMN IF EXISTS sequence_last_attempt_at;
ALTER TABLE work_items DROP COLUMN IF EXISTS sequence_consecutive_scan_errors;
ALTER TABLE work_items DROP COLUMN IF EXISTS sequence_last_progress_at;
