-- Runtime image structured failure fields (ADR actionable failures).
ALTER TABLE runtime_images
  ADD COLUMN IF NOT EXISTS failure_reason text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS failed_step text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS log_tail text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS failure_category text NOT NULL DEFAULT '';
