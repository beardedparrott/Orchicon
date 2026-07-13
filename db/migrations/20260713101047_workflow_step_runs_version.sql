-- Add "version" column to "workflow_step_runs" for optimistic concurrency
-- (docs/09 §5: every mutable table has a version column). The original
-- Phase 6 migration omitted it; this forward migration adds it. All
-- existing rows get version=1.
ALTER TABLE "workflow_step_runs" ADD COLUMN "version" integer NOT NULL DEFAULT 1;
