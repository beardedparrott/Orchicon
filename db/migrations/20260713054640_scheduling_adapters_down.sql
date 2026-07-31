-- Down migration for 20260713054640_scheduling_adapters.sql

DROP TABLE IF EXISTS "checkpoints" CASCADE;
DROP INDEX IF EXISTS "checkpoints_execution_idx";
DROP TABLE IF EXISTS "runtime_adapters" CASCADE;
DROP INDEX IF EXISTS "runtime_adapters_tenant_kind_idx";
DROP INDEX IF EXISTS "runtime_adapters_tenant_status_idx";
DROP TABLE IF EXISTS "worker_executions" CASCADE;
DROP INDEX IF EXISTS "worker_executions_status_health_idx";
DROP INDEX IF EXISTS "worker_executions_task_idx";
DROP INDEX IF EXISTS "worker_executions_tenant_project_idx";
DROP INDEX IF EXISTS "worker_executions_worker_status_idx";
