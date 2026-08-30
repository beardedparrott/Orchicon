-- Worker plane access: role_ref on the worker header + API key expiry.
--
-- role_ref names the tenant role (roles.id) that grants a worker's plane
-- entitlements. The runtime mint resolves it to a scoped, short-lived API
-- key (api_keys.expires_at) injected into the workflow runtime container,
-- so a role-bound published worker gets `orchicon_plane_*` MCP tools
-- against the real instance — never the sandbox, never a Postgres DSN.
ALTER TABLE "workers" ADD COLUMN IF NOT EXISTS "role_ref" text NOT NULL DEFAULT '';
ALTER TABLE "api_keys" ADD COLUMN IF NOT EXISTS "expires_at" timestamptz NULL;