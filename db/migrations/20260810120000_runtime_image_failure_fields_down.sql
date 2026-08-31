ALTER TABLE runtime_images DROP COLUMN IF EXISTS failure_category;
ALTER TABLE runtime_images DROP COLUMN IF EXISTS log_tail;
ALTER TABLE runtime_images DROP COLUMN IF EXISTS failed_step;
ALTER TABLE runtime_images DROP COLUMN IF EXISTS failure_reason;
