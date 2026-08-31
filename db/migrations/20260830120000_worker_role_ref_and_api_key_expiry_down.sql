-- Rollback: drop the worker role_ref + API key expiry columns.
ALTER TABLE "api_keys" DROP COLUMN IF EXISTS "expires_at";
ALTER TABLE "workers" DROP COLUMN IF EXISTS "role_ref";