-- Create "outbox" table
CREATE TABLE "outbox" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "event_id" text NOT NULL,
  "aggregate_type" text NOT NULL,
  "aggregate_id" text NOT NULL,
  "aggregate_version" integer NOT NULL,
  "event_type" text NOT NULL,
  "payload" jsonb NOT NULL,
  "occurred_at" timestamptz NOT NULL DEFAULT now(),
  "published_at" timestamptz NULL,
  "trace_id" text NULL,
  "correlation_id" text NULL,
  PRIMARY KEY ("id")
);
-- Create index "outbox_event_id_idx" to table: "outbox"
CREATE UNIQUE INDEX "outbox_event_id_idx" ON "outbox" ("event_id");
-- Create index "outbox_unpublished_idx" to table: "outbox"
CREATE INDEX "outbox_unpublished_idx" ON "outbox" ("occurred_at") WHERE (published_at IS NULL);
-- Set comment to table: "outbox"
COMMENT ON TABLE "outbox" IS 'Transactional outbox: every mutation writes a row here in the same tx (docs/09 §6, §3.9). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for outbox (docs/09 §8.5)
--
-- The outbox is tenant-scoped so a buggy relay cannot publish another
-- tenant's events. FORCE is set so the table owner (the control plane's
-- DB role) is also subject to the policy — app.tenant_id must be set per
-- transaction or no rows are visible (fail-closed).
-- ---------------------------------------------------------------------------
ALTER TABLE "outbox" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "outbox" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "outbox"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
