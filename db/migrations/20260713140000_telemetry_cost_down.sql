-- Down migration for 20260713140000_telemetry_cost.sql

DROP TABLE IF EXISTS "usage_records" CASCADE;
DROP INDEX IF EXISTS "usage_records_tenant_occurred_idx";
DROP INDEX IF EXISTS "usage_records_tenant_project_idx";
DROP INDEX IF EXISTS "usage_records_execution_idx";
DROP INDEX IF EXISTS "usage_records_tenant_provider_model_idx";
