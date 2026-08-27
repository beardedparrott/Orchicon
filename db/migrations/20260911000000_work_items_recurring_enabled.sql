-- Enable/pause lifecycle for recurring work items.
--
-- recurring_enabled: recurrence on/off flag. false = paused: the item
--   keeps its recurring_schedule + next_run_at (so a pause preserves the
--   schedule and resume re-arms from it) but is excluded from the
--   due-scan. true = firing. Default true.
-- A dedicated column (not inside the schedule JSONB) keeps the due-scan
-- predicate index-friendly on the existing next_run_at partial index and
-- preserves the cursor across a pause.
ALTER TABLE "work_items"
  ADD COLUMN IF NOT EXISTS "recurring_enabled" BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN work_items.recurring_enabled
  IS 'Recurrence on/off. Preserves recurring_schedule + next_run_at while paused so resume re-arms from the schedule.';
