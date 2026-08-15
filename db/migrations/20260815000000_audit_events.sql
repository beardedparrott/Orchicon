-- General-purpose audit trail.
--
-- Append-only record of state-changing events, the first general audit
-- table (today only policy_decisions exists). Each row names the actor,
-- scopes to the tenant, points at a polymorphic target (target_type /
-- target_id span every entity kind, so no FK — enforced by the writer,
-- same as policy_decisions), captures before/after JSONB for mutations,
-- and carries the OTel trace id for end-to-end forensics.
--
-- actor_identity_id is nullable + ON DELETE SET NULL: an audit row is
-- immutable evidence, so deleting an identity must never delete the trail
-- (actor_type still records the kind of actor). trace_id NOT NULL DEFAULT ''
-- matches policy_decisions/telemetry_cost. action is open text, not an
-- enum, so today's outbox event_type vocabulary (e.g. task.succeeded)
-- maps 1:1 onto rows.
--
-- Forward-only (new table, no ADD COLUMN needed). RLS SQL is hand-
-- appended (Atlas free tier does not diff policy blocks) — run
-- `make migrate-hash` after hand-editing.

CREATE TABLE "audit_events" (
  "id"                text NOT NULL,
  "tenant_id"         text NOT NULL,
  "actor_identity_id" text NULL,
  "actor_type"        text NOT NULL DEFAULT 'system',
  "action"            text NOT NULL,
  "target_type"       text NOT NULL,
  "target_id"         text NOT NULL,
  "before"            jsonb NOT NULL DEFAULT '{}',
  "after"             jsonb NOT NULL DEFAULT '{}',
  "trace_id"          text NOT NULL DEFAULT '',
  "occurred_at"       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);

-- Newest-first per-action browsing (backward index scan covers
-- ORDER BY occurred_at DESC).
CREATE INDEX "audit_events_tenant_action_idx"
  ON "audit_events" ("tenant_id", "action", "occurred_at");

-- Entity history: "all events touching this row".
CREATE INDEX "audit_events_tenant_target_idx"
  ON "audit_events" ("tenant_id", "target_type", "target_id");

-- "What did this actor do."
CREATE INDEX "audit_events_tenant_actor_idx"
  ON "audit_events" ("tenant_id", "actor_identity_id", "occurred_at");

-- Cross-service correlation via OTel; mirrors policy_decisions_trace_idx.
CREATE INDEX "audit_events_trace_idx"
  ON "audit_events" ("trace_id");

-- Deleting an identity nulls the actor ref without losing the row (repo
-- invariant: every REFERENCES is SET NULL or CASCADE).
ALTER TABLE "audit_events"
  ADD CONSTRAINT "audit_events_actor_identity_fk" FOREIGN KEY ("actor_identity_id")
  REFERENCES "identities" ("id") ON DELETE SET NULL;

-- Set comment to table: "audit_events"
COMMENT ON TABLE "audit_events" IS 'Append-only audit trail of state-changing events (actor, tenant, polymorphic target, before/after, trace_id). RLS-enabled.';

-- Row-level security: the uniform tenant_isolation policy (docs/09 §8.5).
-- The data-access layer is the primary isolation layer; RLS is the
-- backstop — even a buggy query cannot leak an audit event across tenants.
ALTER TABLE "audit_events" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "audit_events" FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON "audit_events"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
