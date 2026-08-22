-- Local-account credentials for the embedded OpenID Provider.
--
-- Human passwords for the embedded IdP's local accounts. ONLY the
-- password hash is stored (a self-describing PHC string: argon2id
-- $argon2id$v=19$m=…,t=…,p=…$… or bcrypt $2a$/$2b$/$2y$); plaintext is
-- never persisted, logged, or returned. Written/read exclusively by the
-- identity-provider boundary (internal/auth + internal/auth/op) — no
-- control-plane service, RPC, or Ask Orchicon tool touches this table.
--
-- Forward-only (new table, no ADD COLUMN needed). RLS SQL is hand-
-- appended (Atlas free tier does not diff policy blocks) — run
-- `make migrate-hash` after hand-editing.

CREATE TABLE "local_credentials" (
  "id"            text NOT NULL,
  "tenant_id"     text NOT NULL,
  "identity_id"   text NOT NULL,
  "username"      text NOT NULL,
  "password_hash" text NOT NULL,
  "status"        text NOT NULL DEFAULT 'active',
  "version"       integer NOT NULL DEFAULT 1,
  "created_at"    timestamptz NOT NULL DEFAULT now(),
  "updated_at"    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);

-- One local credential per identity within a tenant.
CREATE UNIQUE INDEX "local_credentials_tenant_identity_idx"
  ON "local_credentials" ("tenant_id", "identity_id");

-- The username is the login handle the human types; unique per tenant so
-- login resolves with a single lookup.
CREATE UNIQUE INDEX "local_credentials_tenant_username_idx"
  ON "local_credentials" ("tenant_id", "username");

-- Deleting an identity cascades to its local credential (repo invariant:
-- every REFERENCES is SET NULL or CASCADE).
ALTER TABLE "local_credentials"
  ADD CONSTRAINT "local_credentials_identity_fk" FOREIGN KEY ("identity_id")
  REFERENCES "identities" ("id") ON DELETE CASCADE;

-- Row-level security: the uniform tenant_isolation policy (docs/09 §8.5).
-- The data-access layer is the primary isolation layer; RLS is the
-- backstop — even a buggy query cannot leak a credential across tenants.
ALTER TABLE "local_credentials" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "local_credentials" FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON "local_credentials"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
