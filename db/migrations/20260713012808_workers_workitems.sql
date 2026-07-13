-- Create "edit_locks" table
CREATE TABLE "edit_locks" (
  "resource_id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "resource_type" text NOT NULL DEFAULT 'worker',
  "held_by" text NOT NULL,
  "acquired_at" timestamptz NOT NULL DEFAULT now(),
  "expires_at" timestamptz NOT NULL,
  PRIMARY KEY ("resource_id", "resource_type")
);
-- Set comment to table: "edit_locks"
COMMENT ON TABLE "edit_locks" IS 'Advisory edit lock for the visual editor (docs/07 §3.3). RLS-enabled.';
-- Create "work_item_dependencies" table
CREATE TABLE "work_item_dependencies" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL,
  "from_id" text NOT NULL,
  "to_id" text NOT NULL,
  "type" text NOT NULL DEFAULT 'depends_on',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "work_item_deps_from_idx" to table: "work_item_dependencies"
CREATE INDEX "work_item_deps_from_idx" ON "work_item_dependencies" ("from_id");
-- Create index "work_item_deps_pair_idx" to table: "work_item_dependencies"
CREATE UNIQUE INDEX "work_item_deps_pair_idx" ON "work_item_dependencies" ("from_id", "to_id", "type");
-- Create index "work_item_deps_project_idx" to table: "work_item_dependencies"
CREATE INDEX "work_item_deps_project_idx" ON "work_item_dependencies" ("project_id");
-- Create index "work_item_deps_to_idx" to table: "work_item_dependencies"
CREATE INDEX "work_item_deps_to_idx" ON "work_item_dependencies" ("to_id");
-- Set comment to table: "work_item_dependencies"
COMMENT ON TABLE "work_item_dependencies" IS 'DAG edges between work items (docs/02 §2.2, docs/09 §3.2). Cycles rejected at admission. RLS-enabled.';
-- Create "work_items" table
CREATE TABLE "work_items" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "project_id" text NOT NULL,
  "parent_id" text NULL,
  "kind" text NOT NULL,
  "title" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "acceptance_criteria" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'pending',
  "assigned_worker_ref" jsonb NULL,
  "workflow_id" text NULL,
  "priority" integer NOT NULL DEFAULT 0,
  "budgets" jsonb NOT NULL DEFAULT '{}',
  "context_window" integer NOT NULL DEFAULT 0,
  "results" jsonb NOT NULL DEFAULT '{}',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "work_items_project_parent_idx" to table: "work_items"
CREATE INDEX "work_items_project_parent_idx" ON "work_items" ("project_id", "parent_id");
-- Create index "work_items_project_status_priority_idx" to table: "work_items"
CREATE INDEX "work_items_project_status_priority_idx" ON "work_items" ("project_id", "status", "priority");
-- Create index "work_items_tenant_status_idx" to table: "work_items"
CREATE INDEX "work_items_tenant_status_idx" ON "work_items" ("tenant_id", "status");
-- Set comment to table: "work_items"
COMMENT ON TABLE "work_items" IS 'Work hierarchy: epic/feature/task/subtask (docs/02 §2.2, docs/09 §3.2). RLS-enabled.';
-- Create "worker_versions" table
CREATE TABLE "worker_versions" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "worker_id" text NOT NULL,
  "version" integer NOT NULL,
  "version_note" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'draft',
  "runtime_ref" text NOT NULL DEFAULT '',
  "model_ref" text NOT NULL DEFAULT '',
  "system_prompt" text NOT NULL DEFAULT '',
  "context_sources" jsonb NOT NULL DEFAULT '[]',
  "permissions" jsonb NOT NULL DEFAULT '{}',
  "gated_tools" jsonb NOT NULL DEFAULT '[]',
  "budget_overrides" jsonb NOT NULL DEFAULT '{}',
  "execution_policy_ref" text NOT NULL DEFAULT '',
  "concurrency_limit" integer NOT NULL DEFAULT 1,
  "recovery_workflow_ref" text NOT NULL DEFAULT '',
  "labels" jsonb NOT NULL DEFAULT '{}',
  "published_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "worker_versions_tenant_status_idx" to table: "worker_versions"
CREATE INDEX "worker_versions_tenant_status_idx" ON "worker_versions" ("tenant_id", "status");
-- Create index "worker_versions_worker_version_idx" to table: "worker_versions"
CREATE UNIQUE INDEX "worker_versions_worker_version_idx" ON "worker_versions" ("worker_id", "version");
-- Set comment to table: "worker_versions"
COMMENT ON TABLE "worker_versions" IS 'Worker version snapshot: immutable once published (docs/05 §5, docs/09 §3.3). RLS-enabled.';
-- Create "workers" table
CREATE TABLE "workers" (
  "id" text NOT NULL,
  "tenant_id" text NOT NULL,
  "name" text NOT NULL,
  "slug" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "purpose" text NOT NULL DEFAULT '',
  "status" text NOT NULL DEFAULT 'draft',
  "current_version" integer NOT NULL DEFAULT 0,
  "created_by" text NOT NULL DEFAULT '',
  "version" integer NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "workers_tenant_slug_idx" to table: "workers"
CREATE UNIQUE INDEX "workers_tenant_slug_idx" ON "workers" ("tenant_id", "slug");
-- Create index "workers_tenant_status_idx" to table: "workers"
CREATE INDEX "workers_tenant_status_idx" ON "workers" ("tenant_id", "status");
-- Set comment to table: "workers"
COMMENT ON TABLE "workers" IS 'Worker header: reusable, versioned execution profile (docs/05 §3, docs/09 §3.3). RLS-enabled.';

-- ---------------------------------------------------------------------------
-- Row-Level Security for Phase 4 tables (docs/09_Database_Schema.md §8.5)
--
-- Every tenant_id-bearing table gets the uniform policy:
--   USING (tenant_id = current_setting('app.tenant_id', true))
-- FORCE is set so the policy applies to the table owner too — the
-- control plane's DB role must set app.tenant_id per transaction or see
-- no rows (fail-closed). The data-access layer is the primary isolation
-- layer; RLS is the backstop (docs/09 §8.5).
-- ---------------------------------------------------------------------------

-- workers
ALTER TABLE "workers" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "workers" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "workers"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- worker_versions
ALTER TABLE "worker_versions" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "worker_versions" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "worker_versions"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- work_items
ALTER TABLE "work_items" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "work_items" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "work_items"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- work_item_dependencies
ALTER TABLE "work_item_dependencies" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "work_item_dependencies" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "work_item_dependencies"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- edit_locks
ALTER TABLE "edit_locks" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "edit_locks" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "edit_locks"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));
