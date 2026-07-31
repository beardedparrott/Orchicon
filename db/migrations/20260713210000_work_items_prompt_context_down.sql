-- Down migration for 20260713210000_work_items_prompt_context.sql

ALTER TABLE "work_items" DROP COLUMN IF EXISTS "prompt_context";
