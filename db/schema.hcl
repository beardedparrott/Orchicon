// Orchicon declarative schema (source of truth for table structure).
//
// Mirrors docs/09_Database_Schema.md. The first revision covers the
// Identity & Tenancy and Projects table groups (docs/09 §3.1, §3.2).
//
// Row-Level Security policies are NOT expressed here because Atlas's
// free tier does not diff RLS policies. They are maintained as raw SQL
// appended to the generated migrations (see migrations/). The
// data-access layer is the primary isolation layer; RLS is the
// backstop (docs/09 §8.5). FORCE ROW LEVEL SECURITY is set so the
// policy applies to the table owner too — the control plane's DB role
// must set app.tenant_id per transaction or see no rows.

schema "public" {
}

table "tenants" {
  schema = schema.public
  comment = "Tenant root; budget envelope, default policies (docs/09 §3.1)"

  column "id" {
    type = text
    null = false
  }
  column "slug" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "active"
  }
  column "budget_envelope" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "default_policy_refs" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "tenants_slug_idx" {
    unique  = true
    columns = [column.slug]
  }
}

table "identities" {
  schema = schema.public
  comment = "Users and service accounts within a tenant (docs/09 §3.1). RLS-enabled: see migrations/."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "subject" {
    type = text
    null = false
  }
  column "display_name" {
    type = text
    null = true
  }
  column "status" {
    type = text
    null = false
    default = "active"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "identities_tenant_subject_idx" {
    unique  = true
    columns = [column.tenant_id, column.subject]
  }
}

table "projects" {
  schema = schema.public
  comment = "Top-level tenant of work state (docs/09 §3.2, docs/02 §2.1). RLS-enabled: see migrations/."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "slug" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "drafting"
  }
  column "goals" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "budget_envelope" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "default_policy_refs" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "projects_tenant_slug_idx" {
    unique  = true
    columns = [column.tenant_id, column.slug]
  }
  index "projects_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "outbox" {
  schema = schema.public
  comment = "Transactional outbox: every mutation writes a row here in the same tx (docs/09 §6, §3.9). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "event_id" {
    type = text
    null = false
  }
  column "aggregate_type" {
    type = text
    null = false
  }
  column "aggregate_id" {
    type = text
    null = false
  }
  column "aggregate_version" {
    type = integer
    null = false
  }
  column "event_type" {
    type = text
    null = false
  }
  column "payload" {
    type = jsonb
    null = false
  }
  column "occurred_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "published_at" {
    type = timestamptz
    null = true
  }
  column "trace_id" {
    type = text
    null = true
  }
  column "correlation_id" {
    type = text
    null = true
  }

  primary_key {
    columns = [column.id]
  }

  // Hot path: relay polls unpublished rows ordered by occurrence time
  // (docs/09 §7). Partial index keeps it small as rows get published.
  index "outbox_unpublished_idx" {
    columns = [column.occurred_at]
    where = "published_at IS NULL"
  }
  index "outbox_event_id_idx" {
    unique  = true
    columns = [column.event_id]
  }
}

// --- Phase 4: Workers + Work Items ----------------------------------------
// Workers are tenant-owned, versioned, reusable execution profiles
// (docs/05_Worker_Specification.md). `workers` is the immutable header;
// `worker_versions` holds the mutable snapshot per version. A published
// version is immutable; changes create a new version (docs/05 §4, §5).

table "workers" {
  schema = schema.public
  comment = "Worker header: reusable, versioned execution profile (docs/05 §3, docs/09 §3.3). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "slug" {
    type = text
    null = false
  }
  column "description" {
    type = text
    null = false
    default = ""
  }
  column "purpose" {
    type = text
    null = false
    default = ""
  }
  column "status" {
    type = text
    null = false
    default = "draft"
  }
  column "current_version" {
    type = integer
    null = false
    default = 0
  }
  column "created_by" {
    type = text
    null = false
    default = ""
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "workers_tenant_slug_idx" {
    unique  = true
    columns = [column.tenant_id, column.slug]
  }
  index "workers_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "worker_versions" {
  schema = schema.public
  comment = "Worker version snapshot: immutable once published (docs/05 §5, docs/09 §3.3). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "worker_id" {
    type = text
    null = false
  }
  column "version" {
    type = integer
    null = false
  }
  column "version_note" {
    type = text
    null = false
    default = ""
  }
  column "status" {
    type = text
    null = false
    default = "draft"
  }
  column "runtime_ref" {
    type = text
    null = false
    default = ""
  }
  column "model_ref" {
    type = text
    null = false
    default = ""
  }
  column "system_prompt" {
    type = text
    null = false
    default = ""
  }
  column "context_sources" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "permissions" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "gated_tools" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "budget_overrides" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "execution_policy_ref" {
    type = text
    null = false
    default = ""
  }
  column "concurrency_limit" {
    type = integer
    null = false
    default = 1
  }
  column "recovery_workflow_ref" {
    type = text
    null = false
    default = ""
  }
  column "labels" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "published_at" {
    type = timestamptz
    null = true
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "worker_versions_worker_version_idx" {
    unique  = true
    columns = [column.worker_id, column.version]
  }
  index "worker_versions_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

// Work Items: the Epic → Feature → Task → Subtask hierarchy
// (docs/02_Domain_Model.md §2.2). Dependencies are edges in a DAG.

table "work_items" {
  schema = schema.public
  comment = "Work hierarchy: epic/feature/task/subtask (docs/02 §2.2, docs/09 §3.2). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "project_id" {
    type = text
    null = false
  }
  column "parent_id" {
    type = text
    null = true
  }
  column "kind" {
    type = text
    null = false
  }
  column "title" {
    type = text
    null = false
  }
  column "description" {
    type = text
    null = false
    default = ""
  }
  column "acceptance_criteria" {
    type = text
    null = false
    default = ""
  }
  column "status" {
    type = text
    null = false
    default = "pending"
  }
  column "assigned_worker_ref" {
    type = jsonb
    null = true
  }
  column "workflow_id" {
    type = text
    null = true
  }
  column "priority" {
    type = integer
    null = false
    default = 0
  }
  column "budgets" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "context_window" {
    type = integer
    null = false
    default = 0
  }
  column "results" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "work_items_project_status_priority_idx" {
    columns = [column.project_id, column.status, column.priority]
  }
  index "work_items_project_parent_idx" {
    columns = [column.project_id, column.parent_id]
  }
  index "work_items_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "work_item_dependencies" {
  schema = schema.public
  comment = "DAG edges between work items (docs/02 §2.2, docs/09 §3.2). Cycles rejected at admission. RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "project_id" {
    type = text
    null = false
  }
  column "from_id" {
    type = text
    null = false
  }
  column "to_id" {
    type = text
    null = false
  }
  column "type" {
    type = text
    null = false
    default = "depends_on"
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "work_item_deps_from_idx" {
    columns = [column.from_id]
  }
  index "work_item_deps_to_idx" {
    columns = [column.to_id]
  }
  index "work_item_deps_project_idx" {
    columns = [column.project_id]
  }
  index "work_item_deps_pair_idx" {
    unique  = true
    columns = [column.from_id, column.to_id, column.type]
  }
}

// Edit locks for the visual Worker editor (docs/07 §3.3). Prevents
// concurrent edits; expires automatically on TTL. Shared by the
// WorkerService (and later WorkflowService).
table "edit_locks" {
  schema = schema.public
  comment = "Advisory edit lock for the visual editor (docs/07 §3.3). RLS-enabled."

  column "resource_id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "resource_type" {
    type = text
    null = false
    default = "worker"
  }
  column "held_by" {
    type = text
    null = false
  }
  column "acquired_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "expires_at" {
    type = timestamptz
    null = false
  }

  primary_key {
    columns = [column.resource_id, column.resource_type]
  }
}

// --- Phase 5: Scheduling + Adapters ----------------------------------------
// Runtime adapters are registered sidecar processes offering execution
// capabilities (docs/02 §2.8, docs/04, docs/09 §3.7). WorkerExecutions
// are the concrete invocations created by the TaskReconciler at
// dispatch (docs/02 §2.7, docs/03 §4).

table "runtime_adapters" {
  schema = schema.public
  comment = "Registered adapter process offering execution capabilities (docs/04, docs/09 §3.7). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "kind" {
    type = text
    null = false
  }
  column "version" {
    type = text
    null = false
  }
  column "endpoint" {
    type = text
    null = false
    default = ""
  }
  column "capabilities" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "status" {
    type = text
    null = false
    default = "registered"
  }
  column "max_concurrent_executions" {
    type = integer
    null = false
    default = 1
  }
  column "registered_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "last_heartbeat_at" {
    type = timestamptz
    null = true
  }

  primary_key {
    columns = [column.id]
  }

  index "runtime_adapters_tenant_kind_idx" {
    columns = [column.tenant_id, column.kind]
  }
  index "runtime_adapters_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "worker_executions" {
  schema = schema.public
  comment = "Concrete invocation of a Worker against a Task on an adapter (docs/02 §2.7, docs/09 §3.3). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "project_id" {
    type = text
    null = false
  }
  column "task_id" {
    type = text
    null = false
  }
  column "worker_id" {
    type = text
    null = false
  }
  column "worker_version" {
    type = integer
    null = false
  }
  column "adapter_id" {
    type = text
    null = true
  }
  column "status" {
    type = text
    null = false
    default = "dispatching"
  }
  column "health_state" {
    type = text
    null = false
    default = "healthy"
  }
  column "started_at" {
    type = timestamptz
    null = true
  }
  column "ended_at" {
    type = timestamptz
    null = true
  }
  column "token_usage" {
    type = bigint
    null = false
    default = 0
  }
  column "cost_usd" {
    type = sql("double precision")
    null = false
    default = 0
  }
  column "checkpoint_ref" {
    type = text
    null = true
  }
  column "recovery_id" {
    type = text
    null = true
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "worker_executions_task_idx" {
    columns = [column.task_id]
  }
  index "worker_executions_worker_status_idx" {
    columns = [column.worker_id, column.status]
  }
  index "worker_executions_status_health_idx" {
    columns = [column.status, column.health_state]
  }
  index "worker_executions_tenant_project_idx" {
    columns = [column.tenant_id, column.project_id]
  }
}

table "checkpoints" {
  schema = schema.public
  comment = "Adapter-produced checkpoint blob (docs/04 §5, docs/09 §3.8). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "worker_execution_id" {
    type = text
    null = false
  }
  column "format_version" {
    type = text
    null = false
  }
  column "blob_ref" {
    type = text
    null = false
  }
  column "size_bytes" {
    type = bigint
    null = false
    default = 0
  }
  column "sha256" {
    type = text
    null = false
    default = ""
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "checkpoints_execution_idx" {
    columns = [column.worker_execution_id]
  }
}

// --- Phase 6: Workflows -----------------------------------------------------
// Workflows are composable execution plans referencing Workers and
// Steps (docs/02_Domain_Model.md §2.4, docs/09 §3.4). `workflows` is the
// immutable header; `workflow_versions` holds the steps snapshot per
// version. A published version is immutable; changes create a new
// version. Workflows live at project level (project_id set) or as
// tenant-level templates (project_id empty).

table "workflows" {
  schema = schema.public
  comment = "Workflow header: composable execution plan (docs/02 §2.4, docs/09 §3.4). project_id empty for templates. RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "project_id" {
    type = text
    null = false
    default = ""
  }
  column "name" {
    type = text
    null = false
  }
  column "current_version" {
    type = integer
    null = false
    default = 0
  }
  column "status" {
    type = text
    null = false
    default = "draft"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "workflows_tenant_project_idx" {
    columns = [column.tenant_id, column.project_id]
  }
  index "workflows_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "workflow_versions" {
  schema = schema.public
  comment = "Workflow version snapshot: immutable once published (docs/02 §2.4, docs/09 §3.4). steps is a JSON array of Step messages. RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "workflow_id" {
    type = text
    null = false
  }
  column "version" {
    type = integer
    null = false
  }
  column "version_note" {
    type = text
    null = false
    default = ""
  }
  column "status" {
    type = text
    null = false
    default = "draft"
  }
  column "steps" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "inputs" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "outputs" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "recovery_policy_ref" {
    type = text
    null = false
    default = ""
  }
  column "published_at" {
    type = timestamptz
    null = true
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "workflow_versions_workflow_version_idx" {
    unique  = true
    columns = [column.workflow_id, column.version]
  }
  index "workflow_versions_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "workflow_runs" {
  schema = schema.public
  comment = "A single execution of a published Workflow version (docs/02 §2.4, docs/09 §3.4). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "workflow_id" {
    type = text
    null = false
  }
  column "workflow_version" {
    type = integer
    null = false
  }
  column "project_id" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "pending"
  }
  column "current_step" {
    type = text
    null = false
    default = ""
  }
  column "run_context" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "started_at" {
    type = timestamptz
    null = true
  }
  column "ended_at" {
    type = timestamptz
    null = true
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "workflow_runs_tenant_project_idx" {
    columns = [column.tenant_id, column.project_id]
  }
  index "workflow_runs_workflow_status_idx" {
    columns = [column.workflow_id, column.status]
  }
}

table "workflow_step_runs" {
  schema = schema.public
  comment = "Runtime state of a single step within a WorkflowRun (docs/09 §3.4). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "workflow_run_id" {
    type = text
    null = false
  }
  column "step_id" {
    type = text
    null = false
  }
  column "step_name" {
    type = text
    null = false
    default = ""
  }
  column "step_kind" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "pending"
  }
  column "attempt" {
    type = integer
    null = false
    default = 0
  }
  column "result" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "worker_execution_id" {
    type = text
    null = false
    default = ""
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "started_at" {
    type = timestamptz
    null = true
  }
  column "ended_at" {
    type = timestamptz
    null = true
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "workflow_step_runs_run_idx" {
    columns = [column.workflow_run_id]
  }
  index "workflow_step_runs_run_status_idx" {
    columns = [column.workflow_run_id, column.status]
  }
}
