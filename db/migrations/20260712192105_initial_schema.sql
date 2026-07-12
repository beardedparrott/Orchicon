-- Create "identities" table
CREATE TABLE "identities" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "subject" text NOT NULL,
  "display_name" text NULL,
  "status" text NOT NULL DEFAULT 'active',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "identities_tenant_subject_idx" to table: "identities"
CREATE UNIQUE INDEX "identities_tenant_subject_idx" ON "identities" ("tenant_id", "subject");
-- Set comment to table: "identities"
COMMENT ON TABLE "identities" IS 'Users and service accounts within a tenant (docs/09 §3.1). RLS-enabled: see migrations/.';
-- Create "projects" table
CREATE TABLE "projects" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "name" text NOT NULL,
  "slug" text NOT NULL,
  "status" text NOT NULL DEFAULT 'drafting',
  "goals" jsonb NOT NULL DEFAULT '{}',
  "budget_envelope" jsonb NOT NULL DEFAULT '{}',
  "default_policy_refs" jsonb NOT NULL DEFAULT '[]',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "projects_tenant_slug_idx" to table: "projects"
CREATE UNIQUE INDEX "projects_tenant_slug_idx" ON "projects" ("tenant_id", "slug");
-- Create index "projects_tenant_status_idx" to table: "projects"
CREATE INDEX "projects_tenant_status_idx" ON "projects" ("tenant_id", "status");
-- Set comment to table: "projects"
COMMENT ON TABLE "projects" IS 'Top-level tenant of work state (docs/09 §3.2, docs/02 §2.1). RLS-enabled: see migrations/.';
-- Create "tenants" table
CREATE TABLE "tenants" (
  "id" text NOT NULL,
  "slug" text NOT NULL,
  "name" text NOT NULL,
  "status" text NOT NULL DEFAULT 'active',
  "budget_envelope" jsonb NOT NULL DEFAULT '{}',
  "default_policy_refs" jsonb NOT NULL DEFAULT '[]',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "tenants_slug_idx" to table: "tenants"
CREATE UNIQUE INDEX "tenants_slug_idx" ON "tenants" ("slug");
-- Set comment to table: "tenants"
COMMENT ON TABLE "tenants" IS 'Tenant root; budget envelope, default policies (docs/09 §3.1)';

-- ---------------------------------------------------------------------------
-- Row-Level Security (docs/09_Database_Schema.md §8.5)
--
-- The data-access layer is the primary tenant-isolation layer; RLS is
-- the backstop. Every tenant_id-bearing table gets the uniform policy:
--   USING (tenant_id = current_setting('app.tenant_id', true))
-- The data-access layer sets app.tenant_id per transaction. FORCE is
-- set so the policy applies to the table owner too — the control
-- plane's DB role must set the variable or see no rows (fail-closed).
--
-- The tenants table itself has no tenant_id (it IS the tenant) and is
-- therefore not RLS-enabled.
-- ---------------------------------------------------------------------------

-- identities
ALTER TABLE "identities" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "identities" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "identities"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- projects
ALTER TABLE "projects" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "projects" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "projects"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
