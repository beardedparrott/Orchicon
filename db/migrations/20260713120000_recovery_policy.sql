-- Create "policies" table
CREATE TABLE "policies" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "name" text NOT NULL,
  "current_version" integer NOT NULL DEFAULT 0,
  "status" text NOT NULL DEFAULT 'draft',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "policies_tenant_status_idx" to table: "policies"
CREATE INDEX "policies_tenant_status_idx" ON "policies" ("tenant_id", "status");
-- Set comment to table: "policies"
COMMENT ON TABLE "policies" IS 'Policy header: reusable, versioned rule (docs/02 §2.5, docs/09 §3.5). RLS-enabled.';
-- Create "policy_versions" table
CREATE TABLE "policy_versions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "policy_id" text NOT NULL,
  "version" integer NOT NULL,
  "version_note" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'draft',
  "decision_point" text NOT NULL DEFAULT 'admission',
  "scope" text NOT NULL DEFAULT 'tenant',
  "scope_ref" text NOT NULL DEFAULT '',
  "effect" text NOT NULL DEFAULT 'allow',
  "rego_module" text NOT NULL DEFAULT '',
  "query" text NOT NULL DEFAULT '',
  "published_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "policy_versions_policy_version_idx" to table: "policy_versions"
CREATE UNIQUE INDEX "policy_versions_policy_version_idx" ON "policy_versions" ("policy_id", "version");
-- Create index "policy_versions_tenant_status_idx" to table: "policy_versions"
CREATE INDEX "policy_versions_tenant_status_idx" ON "policy_versions" ("tenant_id", "status");
-- Create index "policy_versions_point_scope_idx" to table: "policy_versions"
CREATE INDEX "policy_versions_point_scope_idx" ON "policy_versions" ("decision_point", "scope", "scope_ref");
-- Set comment to table: "policy_versions"
COMMENT ON TABLE "policy_versions" IS 'Policy version snapshot: immutable once published (docs/02 §2.5, docs/09 §3.5). rego_module is the Rego source. RLS-enabled.';
-- Create "policy_decisions" table
CREATE TABLE "policy_decisions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "policy_id" text NOT NULL DEFAULT '',
  "policy_version" integer NOT NULL DEFAULT 0,
  "decision_point" text NOT NULL,
  "effect" text NOT NULL DEFAULT 'allow',
  "scope" text NOT NULL DEFAULT 'tenant',
  "scope_ref" text NOT NULL DEFAULT '',
  "target_type" text NOT NULL DEFAULT '',
  "target_id" text NOT NULL DEFAULT '',
  "actor_type" text NOT NULL DEFAULT 'system',
  "actor_id" text NOT NULL DEFAULT '',
  "input" jsonb NOT NULL DEFAULT '{}',
  "result" jsonb NOT NULL DEFAULT '{}',
  "trace" jsonb NOT NULL DEFAULT '[]',
  "trace_id" text NOT NULL DEFAULT '',
  "error" text NOT NULL DEFAULT '',
  "occurred_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "policy_decisions_tenant_point_idx" to table: "policy_decisions"
CREATE INDEX "policy_decisions_tenant_point_idx" ON "policy_decisions" ("tenant_id", "decision_point", "occurred_at");
-- Create index "policy_decisions_target_idx" to table: "policy_decisions"
CREATE INDEX "policy_decisions_target_idx" ON "policy_decisions" ("target_type", "target_id");
-- Create index "policy_decisions_trace_idx" to table: "policy_decisions"
CREATE INDEX "policy_decisions_trace_idx" ON "policy_decisions" ("trace_id");
-- Set comment to table: "policy_decisions"
COMMENT ON TABLE "policy_decisions" IS 'Recorded Policy evaluation (docs/02 §2.5, docs/07 §3.5). trace holds the Rego evaluation for ExplainDecision. RLS-enabled.';
-- Create "recovery_executions" table
CREATE TABLE "recovery_executions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL,
  "task_id" text NOT NULL,
  "failed_execution_id" text NOT NULL,
  "recovery_workflow_id" text NOT NULL DEFAULT '',
  "trigger_reason" text NOT NULL,
  "level" integer NOT NULL DEFAULT 1,
  "status" text NOT NULL DEFAULT 'pending',
  "current_step" text NOT NULL DEFAULT '',
  "resumption_path" text NOT NULL DEFAULT 'summarize_resume',
  "budget_tokens_limit" bigint NOT NULL DEFAULT 0,
  "budget_tokens_used" bigint NOT NULL DEFAULT 0,
  "budget_cost_limit_usd" double precision NOT NULL DEFAULT 0,
  "budget_cost_used_usd" double precision NOT NULL DEFAULT 0,
  "budget_relax_fraction" double precision NOT NULL DEFAULT 0,
  "needs_human_approval" boolean NOT NULL DEFAULT false,
  "continuation_plan_id" text NOT NULL DEFAULT '',
  "reviewer_worker_id" text NOT NULL DEFAULT '',
  "summary" text NOT NULL DEFAULT '',
  "version" integer NOT NULL DEFAULT 1,
  "triggered_at" timestamptz NOT NULL DEFAULT now(),
  "ended_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "recovery_executions_tenant_project_idx" to table: "recovery_executions"
CREATE INDEX "recovery_executions_tenant_project_idx" ON "recovery_executions" ("tenant_id", "project_id");
-- Create index "recovery_executions_task_idx" to table: "recovery_executions"
CREATE INDEX "recovery_executions_task_idx" ON "recovery_executions" ("task_id");
-- Create index "recovery_executions_status_idx" to table: "recovery_executions"
CREATE INDEX "recovery_executions_status_idx" ON "recovery_executions" ("status");
-- Set comment to table: "recovery_executions"
COMMENT ON TABLE "recovery_executions" IS 'Recovery workflow run (docs/06 §2, docs/09 §3.6). RLS-enabled.';
-- Create "recovery_step_runs" table
CREATE TABLE "recovery_step_runs" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "recovery_id" text NOT NULL,
  "step_id" text NOT NULL,
  "step_name" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'pending',
  "attempt" integer NOT NULL DEFAULT 0,
  "result" jsonb NOT NULL DEFAULT '{}',
  "worker_execution_id" text NOT NULL DEFAULT '',
  "trigger_reason" text NOT NULL DEFAULT '',
  "affected_ref" text NOT NULL DEFAULT '',
  "adapter_ref" text NOT NULL DEFAULT '',
  "action" text NOT NULL DEFAULT '',
  "started_at" timestamptz NULL,
  "ended_at" timestamptz NULL,
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "recovery_step_runs_recovery_idx" to table: "recovery_step_runs"
CREATE INDEX "recovery_step_runs_recovery_idx" ON "recovery_step_runs" ("recovery_id");
-- Set comment to table: "recovery_step_runs"
COMMENT ON TABLE "recovery_step_runs" IS 'Runtime state of a single step within a RecoveryExecution (docs/06 §3, docs/09 §3.6). Rich narrative fields for the timeline (docs/06 §11). RLS-enabled.';
-- Create "continuation_plans" table
CREATE TABLE "continuation_plans" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "recovery_id" text NOT NULL,
  "version" integer NOT NULL DEFAULT 1,
  "completed" jsonb NOT NULL DEFAULT '[]',
  "in_progress" jsonb NOT NULL DEFAULT '[]',
  "remaining" jsonb NOT NULL DEFAULT '[]',
  "corrections" jsonb NOT NULL DEFAULT '[]',
  "context_summary" text NOT NULL DEFAULT '',
  "checkpoint_ref" text NOT NULL DEFAULT '',
  "assumptions" jsonb NOT NULL DEFAULT '[]',
  "status" text NOT NULL DEFAULT 'pending',
  "approved_by" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "decided_at" timestamptz NULL,
  PRIMARY KEY ("id")
);
-- Create index "continuation_plans_recovery_idx" to table: "continuation_plans"
CREATE INDEX "continuation_plans_recovery_idx" ON "continuation_plans" ("recovery_id");
-- Set comment to table: "continuation_plans"
COMMENT ON TABLE "continuation_plans" IS 'Continuation plan produced by the plan step (docs/06 §8, docs/09 §3.6). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for Phase 7 tables (docs/09_Database_Schema.md §8.5)
-- Every tenant_id-bearing table gets the uniform policy:
--   USING (tenant_id = current_setting('app.tenant_id', true))
-- FORCE is set so the policy applies to the table owner too.
-- ---------------------------------------------------------------------------

-- policies
ALTER TABLE "policies" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "policies" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "policies"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- policy_versions
ALTER TABLE "policy_versions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "policy_versions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "policy_versions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- policy_decisions
ALTER TABLE "policy_decisions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "policy_decisions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "policy_decisions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- recovery_executions
ALTER TABLE "recovery_executions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "recovery_executions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "recovery_executions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- recovery_step_runs
ALTER TABLE "recovery_step_runs" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "recovery_step_runs" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "recovery_step_runs"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- continuation_plans
ALTER TABLE "continuation_plans" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "continuation_plans" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "continuation_plans"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
