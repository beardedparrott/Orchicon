-- Create "usage_records" table
CREATE TABLE "usage_records" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL DEFAULT '',
  "task_id" text NOT NULL DEFAULT '',
  "execution_id" text NOT NULL DEFAULT '',
  "worker_id" text NOT NULL DEFAULT '',
  "provider" text NOT NULL,
  "model" text NOT NULL,
  "prompt_tokens" bigint NOT NULL DEFAULT 0,
  "completion_tokens" bigint NOT NULL DEFAULT 0,
  "total_tokens" bigint NOT NULL DEFAULT 0,
  "cost_usd" double precision NOT NULL DEFAULT 0,
  "correlation_id" text NOT NULL DEFAULT '',
  "trace_id" text NOT NULL DEFAULT '',
  "occurred_at" timestamptz NOT NULL DEFAULT now(),
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "usage_records_tenant_occurred_idx" to table: "usage_records"
CREATE INDEX "usage_records_tenant_occurred_idx" ON "usage_records" ("tenant_id", "occurred_at" DESC);
-- Create index "usage_records_tenant_project_idx" to table: "usage_records"
CREATE INDEX "usage_records_tenant_project_idx" ON "usage_records" ("tenant_id", "project_id", "occurred_at" DESC);
-- Create index "usage_records_execution_idx" to table: "usage_records"
CREATE INDEX "usage_records_execution_idx" ON "usage_records" ("execution_id");
-- Create index "usage_records_tenant_provider_model_idx" to table: "usage_records"
CREATE INDEX "usage_records_tenant_provider_model_idx" ON "usage_records" ("tenant_id", "provider", "model", "occurred_at" DESC);
-- Set comment to table: "usage_records"
COMMENT ON TABLE "usage_records" IS 'Usage + cost records (docs/08 §5.2, docs/09 §3.7). Postgres is source of truth; mirrored to ClickHouse as OTel metrics (orchicon_tokens_consumed, orchicon_cost_usd) via the dual-write. Cost attribution rolls up Tenant→Project→Task→Execution (docs/10 §11). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for Phase 8 tables (docs/09_Database_Schema.md §8.5)
-- Every tenant_id-bearing table gets the uniform policy:
--   USING (tenant_id = current_setting('app.tenant_id', true))
-- FORCE is set so the policy applies to the table owner too.
-- ---------------------------------------------------------------------------

-- usage_records
ALTER TABLE "usage_records" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "usage_records" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "usage_records"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
