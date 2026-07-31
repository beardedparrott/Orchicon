-- Down migration for 20260712192105_initial_schema.sql

DROP TABLE IF EXISTS "identities" CASCADE;
DROP INDEX IF EXISTS "identities_tenant_subject_idx";
DROP TABLE IF EXISTS "projects" CASCADE;
DROP INDEX IF EXISTS "projects_tenant_slug_idx";
DROP INDEX IF EXISTS "projects_tenant_status_idx";
DROP TABLE IF EXISTS "tenants" CASCADE;
DROP INDEX IF EXISTS "tenants_slug_idx";
