-- Down migration for 20260713160000_auth_webhooks.sql

ALTER TABLE "identities" DROP COLUMN IF EXISTS "identity_type";
DROP TABLE IF EXISTS "roles" CASCADE;
DROP INDEX IF EXISTS "roles_tenant_idx";
DROP INDEX IF EXISTS "roles_tenant_name_idx";
DROP TABLE IF EXISTS "role_bindings" CASCADE;
DROP INDEX IF EXISTS "role_bindings_identity_idx";
DROP INDEX IF EXISTS "role_bindings_role_idx";
DROP INDEX IF EXISTS "role_bindings_tenant_idx";
DROP TABLE IF EXISTS "api_keys" CASCADE;
DROP INDEX IF EXISTS "api_keys_tenant_idx";
DROP INDEX IF EXISTS "api_keys_identity_idx";
DROP INDEX IF EXISTS "api_keys_hash_idx";
DROP TABLE IF EXISTS "event_subscriptions" CASCADE;
DROP INDEX IF EXISTS "event_subscriptions_tenant_idx";
DROP INDEX IF EXISTS "event_subscriptions_status_idx";
DROP TABLE IF EXISTS "webhook_deliveries" CASCADE;
DROP INDEX IF EXISTS "webhook_deliveries_subscription_idx";
DROP INDEX IF EXISTS "webhook_deliveries_tenant_idx";
DROP INDEX IF EXISTS "webhook_deliveries_status_idx";
DROP INDEX IF EXISTS "webhook_deliveries_retry_idx";
