-- audit_events.auth_method: how the actor authenticated.
--
-- The requirement "capture actor (identity id + auth method)" has no
-- column today: actor_type only distinguishes user vs system. This
-- column records the credential kind used ("oidc" | "apikey" | "local" |
-- "signup" | "dev" | "refresh" | "system" | ""), where "" is reserved for
-- legacy pre-migration rows (the NOT NULL DEFAULT '' backfills existing
-- rows on ADD COLUMN). Every row this feature writes carries a real
-- method. No new index: the existing audit_events_tenant_actor_idx
-- covers actor queries and auth_method is never a leading filter column.
--
-- Forward-only (ADD COLUMN IF NOT EXISTS — mandatory per AGENTS.md).
-- RLS SQL is unchanged; run `make migrate-hash` after hand-editing.

ALTER TABLE "audit_events"
  ADD COLUMN IF NOT EXISTS "auth_method" text NOT NULL DEFAULT '';

COMMENT ON TABLE "audit_events" IS 'Append-only audit trail of state-changing events (actor, auth method, tenant, polymorphic target, before/after, trace_id). RLS-enabled.';
