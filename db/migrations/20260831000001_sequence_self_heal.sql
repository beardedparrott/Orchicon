ALTER TABLE work_items
  ADD COLUMN IF NOT EXISTS sequence_attempts INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS sequence_last_attempt_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS sequence_consecutive_scan_errors INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS sequence_last_progress_at TIMESTAMPTZ;
