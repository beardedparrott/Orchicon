-- Down migration for 20260713120000_recovery_policy.sql

DROP TABLE IF EXISTS "policies" CASCADE;
DROP INDEX IF EXISTS "policies_tenant_status_idx";
DROP TABLE IF EXISTS "policy_versions" CASCADE;
DROP INDEX IF EXISTS "policy_versions_policy_version_idx";
DROP INDEX IF EXISTS "policy_versions_tenant_status_idx";
DROP INDEX IF EXISTS "policy_versions_point_scope_idx";
DROP TABLE IF EXISTS "policy_decisions" CASCADE;
DROP INDEX IF EXISTS "policy_decisions_tenant_point_idx";
DROP INDEX IF EXISTS "policy_decisions_target_idx";
DROP INDEX IF EXISTS "policy_decisions_trace_idx";
DROP TABLE IF EXISTS "recovery_executions" CASCADE;
DROP INDEX IF EXISTS "recovery_executions_tenant_project_idx";
DROP INDEX IF EXISTS "recovery_executions_task_idx";
DROP INDEX IF EXISTS "recovery_executions_status_idx";
DROP TABLE IF EXISTS "recovery_step_runs" CASCADE;
DROP INDEX IF EXISTS "recovery_step_runs_recovery_idx";
DROP TABLE IF EXISTS "continuation_plans" CASCADE;
DROP INDEX IF EXISTS "continuation_plans_recovery_idx";
