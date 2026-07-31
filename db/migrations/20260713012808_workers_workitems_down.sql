-- Down migration for 20260713012808_workers_workitems.sql

DROP TABLE IF EXISTS "edit_locks" CASCADE;
DROP TABLE IF EXISTS "work_item_dependencies" CASCADE;
DROP INDEX IF EXISTS "work_item_deps_from_idx";
DROP INDEX IF EXISTS "work_item_deps_pair_idx";
DROP INDEX IF EXISTS "work_item_deps_project_idx";
DROP INDEX IF EXISTS "work_item_deps_to_idx";
DROP TABLE IF EXISTS "work_items" CASCADE;
DROP INDEX IF EXISTS "work_items_project_parent_idx";
DROP INDEX IF EXISTS "work_items_project_status_priority_idx";
DROP INDEX IF EXISTS "work_items_tenant_status_idx";
DROP TABLE IF EXISTS "worker_versions" CASCADE;
DROP INDEX IF EXISTS "worker_versions_tenant_status_idx";
DROP INDEX IF EXISTS "worker_versions_worker_version_idx";
DROP TABLE IF EXISTS "workers" CASCADE;
DROP INDEX IF EXISTS "workers_tenant_slug_idx";
DROP INDEX IF EXISTS "workers_tenant_status_idx";
