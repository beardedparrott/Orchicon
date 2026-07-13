-- Phase 9: Auth + Webhooks (docs/07 §6, §3.11, docs/09 §3.1, §3.9).
--
-- Adds the RBAC tables (roles, role_bindings, api_keys), the webhook
-- tables (event_subscriptions, webhook_deliveries), and an
-- identity_type column on the existing identities table. All
-- tenant_id-bearing tables get the uniform RLS policy (docs/09 §8.5).

-- Extend identities with an identity_type column (user vs service).
ALTER TABLE "identities" ADD COLUMN "identity_type" text NOT NULL DEFAULT 'user';

-- roles: named bundle of entitlements, tenant-scoped (optionally project-scoped).
CREATE TABLE "roles" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "name" text NOT NULL,
  "scope" text NOT NULL DEFAULT 'tenant',
  "scope_ref" text NOT NULL DEFAULT '',
  "entitlements" jsonb NOT NULL DEFAULT '[]',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
CREATE INDEX "roles_tenant_idx" ON "roles" ("tenant_id");
CREATE INDEX "roles_tenant_name_idx" ON "roles" ("tenant_id", "name");
COMMENT ON TABLE "roles" IS 'RBAC role: named bundle of entitlements (docs/07 §6.2). RLS-enabled.';

-- role_bindings: attaches a Role to an Identity within a scope.
CREATE TABLE "role_bindings" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "identity_id" text NOT NULL,
  "role_id" text NOT NULL,
  "scope" text NOT NULL DEFAULT 'tenant',
  "scope_ref" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
CREATE INDEX "role_bindings_identity_idx" ON "role_bindings" ("identity_id");
CREATE INDEX "role_bindings_role_idx" ON "role_bindings" ("role_id");
CREATE INDEX "role_bindings_tenant_idx" ON "role_bindings" ("tenant_id");
COMMENT ON TABLE "role_bindings" IS 'RBAC role binding (docs/07 §6.2). RLS-enabled.';

-- api_keys: hashed machine credentials with scoped entitlements.
CREATE TABLE "api_keys" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "identity_id" text NOT NULL,
  "name" text NOT NULL,
  "key_prefix" text NOT NULL,
  "key_hash" text NOT NULL,
  "scopes" jsonb NOT NULL DEFAULT '[]',
  "status" text NOT NULL DEFAULT 'active',
  "last_used_at" timestamptz NULL,
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
CREATE INDEX "api_keys_tenant_idx" ON "api_keys" ("tenant_id");
CREATE INDEX "api_keys_identity_idx" ON "api_keys" ("identity_id");
CREATE INDEX "api_keys_hash_idx" ON "api_keys" ("key_hash");
COMMENT ON TABLE "api_keys" IS 'Hashed API keys with scoped entitlements (docs/07 §6.1). RLS-enabled.';

-- event_subscriptions: webhook delivery targets.
CREATE TABLE "event_subscriptions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "name" text NOT NULL,
  "target_url" text NOT NULL,
  "event_filter" text NOT NULL DEFAULT '*',
  "scope" text NOT NULL DEFAULT 'tenant',
  "scope_ref" text NOT NULL DEFAULT '',
  "secret_hint" text NOT NULL DEFAULT '',
  "secret_hash" text NOT NULL DEFAULT '',
  "max_retries" integer NOT NULL DEFAULT 5,
  "status" text NOT NULL DEFAULT 'active',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
CREATE INDEX "event_subscriptions_tenant_idx" ON "event_subscriptions" ("tenant_id");
CREATE INDEX "event_subscriptions_status_idx" ON "event_subscriptions" ("status");
COMMENT ON TABLE "event_subscriptions" IS 'Webhook delivery subscription (docs/07 §3.11, docs/09 §3.9). RLS-enabled.';

-- webhook_deliveries: delivery attempts (delivered / retrying / dead_letter).
CREATE TABLE "webhook_deliveries" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "subscription_id" text NOT NULL,
  "event_id" text NOT NULL,
  "event_type" text NOT NULL,
  "payload" jsonb NOT NULL DEFAULT '{}',
  "attempt" integer NOT NULL DEFAULT 0,
  "status_code" integer NOT NULL DEFAULT 0,
  "status" text NOT NULL DEFAULT 'retrying',
  "error" text NOT NULL DEFAULT '',
  "next_attempt_at" timestamptz NULL,
  "occurred_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
CREATE INDEX "webhook_deliveries_subscription_idx" ON "webhook_deliveries" ("subscription_id");
CREATE INDEX "webhook_deliveries_tenant_idx" ON "webhook_deliveries" ("tenant_id");
CREATE INDEX "webhook_deliveries_status_idx" ON "webhook_deliveries" ("status");
CREATE INDEX "webhook_deliveries_retry_idx" ON "webhook_deliveries" ("status", "next_attempt_at");
COMMENT ON TABLE "webhook_deliveries" IS 'Webhook delivery attempt (docs/07 §3.11). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security (docs/09 §8.5). Every tenant_id-bearing table gets
-- the uniform policy; FORCE applies it to the table owner too.
-- ---------------------------------------------------------------------------

-- roles
ALTER TABLE "roles" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "roles" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "roles"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- role_bindings
ALTER TABLE "role_bindings" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "role_bindings" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "role_bindings"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- api_keys
ALTER TABLE "api_keys" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "api_keys" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "api_keys"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- event_subscriptions
ALTER TABLE "event_subscriptions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "event_subscriptions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "event_subscriptions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- webhook_deliveries
ALTER TABLE "webhook_deliveries" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "webhook_deliveries" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "webhook_deliveries"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
