-- Recurring work items: schedule definition + next-occurrence cursor.
--
-- recurring_schedule: JSONB holding the recurrence definition
--   {frequency, interval, days[], start_date, start_time}.
--   NULL = not recurring.
-- next_run_at: computed next occurrence (scheduler due-scan cursor).
--   NULL = not recurring / no next occurrence yet.
-- Same table -> no new RLS policy needed (row-level tenant_isolation RLS
-- already exists on work_items).
ALTER TABLE "work_items"
  ADD COLUMN IF NOT EXISTS "recurring_schedule" JSONB NULL,
  ADD COLUMN IF NOT EXISTS "next_run_at" TIMESTAMPTZ NULL;

-- Due-scan: scheduler finds due recurring items with
-- next_run_at <= now (partial index keeps the scan small).
CREATE INDEX IF NOT EXISTS idx_work_items_tenant_next_run_at
  ON "work_items" ("tenant_id", "next_run_at")
  WHERE "next_run_at" IS NOT NULL;

COMMENT ON COLUMN work_items.recurring_schedule
  IS 'Recurrence definition {frequency, interval, days[], start_date, start_time}. NULL = not recurring.';
COMMENT ON COLUMN work_items.next_run_at
  IS 'Computed next occurrence of a recurring item, used by the scheduler due-scan. NULL = not recurring or no next occurrence.';
