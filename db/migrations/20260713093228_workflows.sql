-- Create "workflows" table
CREATE TABLE "workflows" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL DEFAULT '',
  "name" text NOT NULL,
  "current_version" integer NOT NULL DEFAULT 0,
  "status" text NOT NULL DEFAULT 'draft',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "workflows_tenant_project_idx" to table: "workflows"
CREATE INDEX "workflows_tenant_project_idx" ON "workflows" ("tenant_id", "project_id");
-- Create index "workflows_tenant_status_idx" to table: "workflows"
CREATE INDEX "workflows_tenant_status_idx" ON "workflows" ("tenant_id", "status");
-- Set comment to table: "workflows"
COMMENT ON TABLE "workflows" IS 'Workflow header: composable execution plan (docs/02 §2.4, docs/09 §3.4). project_id empty for templates. RLS-enabled.';
-- Create "workflow_versions" table
CREATE TABLE "workflow_versions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "workflow_id" text NOT NULL,
  "version" integer NOT NULL,
  "version_note" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'draft',
  "steps" jsonb NOT NULL DEFAULT '[]',
  "inputs" jsonb NOT NULL DEFAULT '{}',
  "outputs" jsonb NOT NULL DEFAULT '{}',
  "recovery_policy_ref" text NOT NULL DEFAULT '',
  "published_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "workflow_versions_workflow_version_idx" to table: "workflow_versions"
CREATE UNIQUE INDEX "workflow_versions_workflow_version_idx" ON "workflow_versions" ("workflow_id", "version");
-- Create index "workflow_versions_tenant_status_idx" to table: "workflow_versions"
CREATE INDEX "workflow_versions_tenant_status_idx" ON "workflow_versions" ("tenant_id", "status");
-- Set comment to table: "workflow_versions"
COMMENT ON TABLE "workflow_versions" IS 'Workflow version snapshot: immutable once published (docs/02 §2.4, docs/09 §3.4). steps is a JSON array of Step messages. RLS-enabled.';
-- Create "workflow_runs" table
CREATE TABLE "workflow_runs" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "workflow_id" text NOT NULL,
  "workflow_version" integer NOT NULL,
  "project_id" text NOT NULL,
  "status" text NOT NULL DEFAULT 'pending',
  "current_step" text NOT NULL DEFAULT '',
  "run_context" jsonb NOT NULL DEFAULT '{}',
  "version" integer NOT NULL DEFAULT 1,
  "started_at" timestamptz NULL,
  "ended_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "workflow_runs_tenant_project_idx" to table: "workflow_runs"
CREATE INDEX "workflow_runs_tenant_project_idx" ON "workflow_runs" ("tenant_id", "project_id");
-- Create index "workflow_runs_workflow_status_idx" to table: "workflow_runs"
CREATE INDEX "workflow_runs_workflow_status_idx" ON "workflow_runs" ("workflow_id", "status");
-- Set comment to table: "workflow_runs"
COMMENT ON TABLE "workflow_runs" IS 'A single execution of a published Workflow version (docs/02 §2.4, docs/09 §3.4). RLS-enabled.';
-- Create "workflow_step_runs" table
CREATE TABLE "workflow_step_runs" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "workflow_run_id" text NOT NULL,
  "step_id" text NOT NULL,
  "step_name" text NOT NULL DEFAULT '',
  "step_kind" text NOT NULL,
  "status" text NOT NULL DEFAULT 'pending',
  "attempt" integer NOT NULL DEFAULT 0,
  "result" jsonb NOT NULL DEFAULT '{}',
  "worker_execution_id" text NOT NULL DEFAULT '',
  "started_at" timestamptz NULL,
  "ended_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "workflow_step_runs_run_idx" to table: "workflow_step_runs"
CREATE INDEX "workflow_step_runs_run_idx" ON "workflow_step_runs" ("workflow_run_id");
-- Create index "workflow_step_runs_run_status_idx" to table: "workflow_step_runs"
CREATE INDEX "workflow_step_runs_run_status_idx" ON "workflow_step_runs" ("workflow_run_id", "status");
-- Set comment to table: "workflow_step_runs"
COMMENT ON TABLE "workflow_step_runs" IS 'Runtime state of a single step within a WorkflowRun (docs/09 §3.4). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for Phase 6 tables (docs/09_Database_Schema.md §8.5)
--
-- Every tenant_id-bearing table gets the uniform policy:
--   USING (tenant_id = current_setting('app.tenant_id', true))
-- FORCE is set so the policy applies to the table owner too — the
-- control plane's DB role must set app.tenant_id per transaction or see
-- no rows (fail-closed). The data-access layer is the primary isolation
-- layer; RLS is the backstop (docs/09 §8.5).
-- ---------------------------------------------------------------------------

-- workflows
ALTER TABLE "workflows" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "workflows" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "workflows"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- workflow_versions
ALTER TABLE "workflow_versions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "workflow_versions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "workflow_versions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- workflow_runs
ALTER TABLE "workflow_runs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "workflow_runs" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "workflow_runs"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- workflow_step_runs
ALTER TABLE "workflow_step_runs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "workflow_step_runs" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "workflow_step_runs"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
