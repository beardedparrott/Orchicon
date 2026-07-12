-- Orchicon Postgres init for local dev.
--
-- Creates the application role and database. Migrations are applied
-- separately by the control plane on boot (docs/09 §8) or via
-- `make migrate`. The control plane's DB role is NOT a superuser and
-- never has BYPASSRLS (docs/09 §8.5, invariant #7); here it owns the
-- schema so FORCE ROW LEVEL SECURITY is what makes RLS apply to it.

-- The default role/db come from POSTGRES_USER/POSTGRES_DB env vars
-- (orchicon/orchicon). Nothing extra to create for local dev; this
-- file is a placeholder for future seed data and role hardening.
SELECT 1;

-- ---------------------------------------------------------------------------
-- Dev seed: a default tenant so the control plane and UI have a tenant
-- context to work with before auth (Phase 9) is implemented. The control
-- plane's dev tenant-resolution middleware reads this ID from the
-- x-orchicon-tenant-id header. This is DEV ONLY — production tenants are
-- provisioned through the Admin surface.
--
-- ON CONFLICT keeps this idempotent if the file is re-run. The tenants
-- table has no tenant_id column (it IS the tenant) so no RLS applies.
-- ---------------------------------------------------------------------------
INSERT INTO tenants (id, slug, name, status)
VALUES ('tnt_dev', 'dev', 'Development Tenant', 'active')
ON CONFLICT (id) DO NOTHING;
