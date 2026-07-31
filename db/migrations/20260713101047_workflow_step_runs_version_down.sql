-- Down migration for 20260713101047_workflow_step_runs_version.sql

ALTER TABLE "workflow_step_runs" DROP COLUMN IF EXISTS "version";
