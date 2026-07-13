-- Create "checkpoints" table
CREATE TABLE "checkpoints" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "worker_execution_id" text NOT NULL,
  "format_version" text NOT NULL,
  "blob_ref" text NOT NULL,
  "size_bytes" bigint NOT NULL DEFAULT 0,
  "sha256" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "checkpoints_execution_idx" to table: "checkpoints"
CREATE INDEX "checkpoints_execution_idx" ON "checkpoints" ("worker_execution_id");
-- Set comment to table: "checkpoints"
COMMENT ON TABLE "checkpoints" IS 'Adapter-produced checkpoint blob (docs/04 §5, docs/09 §3.8). RLS-enabled.';
-- Create "runtime_adapters" table
CREATE TABLE "runtime_adapters" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "kind" text NOT NULL,
  "version" text NOT NULL,
  "endpoint" text NOT NULL DEFAULT '',
  "capabilities" jsonb NOT NULL DEFAULT '{}',
  "status" text NOT NULL DEFAULT 'registered',
  "max_concurrent_executions" integer NOT NULL DEFAULT 1,
  "registered_at" timestamptz NOT NULL DEFAULT now(),
  "last_heartbeat_at" timestamptz NULL,
  PRIMARY KEY ("id")
);
-- Create index "runtime_adapters_tenant_kind_idx" to table: "runtime_adapters"
CREATE INDEX "runtime_adapters_tenant_kind_idx" ON "runtime_adapters" ("tenant_id", "kind");
-- Create index "runtime_adapters_tenant_status_idx" to table: "runtime_adapters"
CREATE INDEX "runtime_adapters_tenant_status_idx" ON "runtime_adapters" ("tenant_id", "status");
-- Set comment to table: "runtime_adapters"
COMMENT ON TABLE "runtime_adapters" IS 'Registered adapter process offering execution capabilities (docs/04, docs/09 §3.7). RLS-enabled.';
-- Create "worker_executions" table
CREATE TABLE "worker_executions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL,
  "task_id" text NOT NULL,
  "worker_id" text NOT NULL,
  "worker_version" integer NOT NULL,
  "adapter_id" text NULL,
  "status" text NOT NULL DEFAULT 'dispatching',
  "health_state" text NOT NULL DEFAULT 'healthy',
  "started_at" timestamptz NULL,
  "ended_at" timestamptz NULL,
  "token_usage" bigint NOT NULL DEFAULT 0,
  "cost_usd" double precision NOT NULL DEFAULT 0,
  "checkpoint_ref" text NULL,
  "recovery_id" text NULL,
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "worker_executions_status_health_idx" to table: "worker_executions"
CREATE INDEX "worker_executions_status_health_idx" ON "worker_executions" ("status", "health_state");
-- Create index "worker_executions_task_idx" to table: "worker_executions"
CREATE INDEX "worker_executions_task_idx" ON "worker_executions" ("task_id");
-- Create index "worker_executions_tenant_project_idx" to table: "worker_executions"
CREATE INDEX "worker_executions_tenant_project_idx" ON "worker_executions" ("tenant_id", "project_id");
-- Create index "worker_executions_worker_status_idx" to table: "worker_executions"
CREATE INDEX "worker_executions_worker_status_idx" ON "worker_executions" ("worker_id", "status");
-- Set comment to table: "worker_executions"
COMMENT ON TABLE "worker_executions" IS 'Concrete invocation of a Worker against a Task on an adapter (docs/02 §2.7, docs/09 §3.3). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for Phase 5 tables (docs/09_Database_Schema.md §8.5)
-- ---------------------------------------------------------------------------

-- runtime_adapters
ALTER TABLE "runtime_adapters" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "runtime_adapters" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "runtime_adapters"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- worker_executions
ALTER TABLE "worker_executions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "worker_executions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "worker_executions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- checkpoints
ALTER TABLE "checkpoints" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "checkpoints" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "checkpoints"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
