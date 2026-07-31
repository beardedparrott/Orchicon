-- Down migration for 20260713093228_workflows.sql

DROP TABLE IF EXISTS "workflows" CASCADE;
DROP INDEX IF EXISTS "workflows_tenant_project_idx";
DROP INDEX IF EXISTS "workflows_tenant_status_idx";
DROP TABLE IF EXISTS "workflow_versions" CASCADE;
DROP INDEX IF EXISTS "workflow_versions_workflow_version_idx";
DROP INDEX IF EXISTS "workflow_versions_tenant_status_idx";
DROP TABLE IF EXISTS "workflow_runs" CASCADE;
DROP INDEX IF EXISTS "workflow_runs_tenant_project_idx";
DROP INDEX IF EXISTS "workflow_runs_workflow_status_idx";
DROP TABLE IF EXISTS "workflow_step_runs" CASCADE;
DROP INDEX IF EXISTS "workflow_step_runs_run_idx";
DROP INDEX IF EXISTS "workflow_step_runs_run_status_idx";
