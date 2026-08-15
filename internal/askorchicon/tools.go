package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ToolIntent represents a detected tool call from the user's message.

// ToolFn is the signature for every tool function.
// It receives the tenant-scoped context, the DB pool, and JSON arguments,
// and returns a JSON result or an error.
type ToolFn func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error)

// PropertySchema describes a single field in a tool's input schema.
type PropertySchema struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// ToolDefinition describes a tool available to the agent.
type ToolDefinition struct {
	Name        string
	Description string
	Mutating    bool // if true, requires clarification before execution
	Fn          ToolFn
	Properties  map[string]PropertySchema // input field definitions for MCP schema
	Required    []string                  // required field names for MCP schema
}

// ToolRegistry holds all tools the agent can call.
type ToolRegistry struct {
	tools []ToolDefinition
	byName map[string]ToolDefinition
}

// NewToolRegistry creates the registry with all available tools.
func NewToolRegistry(pool *db.Pool, log *slog.Logger) *ToolRegistry {
	// Package-level logger for tools whose post-commit side effects (e.g.
	// auto-starting a workflow) need a logger but receive none via the
	// ToolFn signature.
	toolLogger = log
	if toolLogger == nil {
		toolLogger = slog.Default()
	}
	r := &ToolRegistry{
		byName: make(map[string]ToolDefinition),
	}

	// Register all tools.
	for _, td := range allTools(pool, log) {
		r.tools = append(r.tools, td)
		r.byName[td.Name] = td
	}

	return r
}

// List returns all registered tool definitions.
func (r *ToolRegistry) List() []ToolDefinition {
	return r.tools
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (ToolDefinition, bool) {
	td, ok := r.byName[name]
	return td, ok
}

// Add registers an additional tool definition (used for tools whose Fn
// depends on service-injected dependencies not available to allTools).
func (r *ToolRegistry) Add(td ToolDefinition) {
	r.tools = append(r.tools, td)
	r.byName[td.Name] = td
}

// IsMutating returns true if the tool performs mutations.
func (r *ToolRegistry) IsMutating(name string) bool {
	td, ok := r.byName[name]
	if !ok {
		return false
	}
	return td.Mutating
}

// Execute runs a tool by name with the given JSON arguments.
func (r *ToolRegistry) Execute(ctx context.Context, pool *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	td, ok := r.byName[name]
	if !ok {
		return nil, nil
	}
	return td.Fn(ctx, pool, args)
}

// allTools returns the complete list of tool definitions.
func allTools(pool *db.Pool, log *slog.Logger) []ToolDefinition {
	return []ToolDefinition{
		// --- Projects ---
		{
			Name:        "list_projects",
			Description: "List all projects for the current tenant. Supports optional search and status filter.",
			Mutating:    false,
			Fn:          toolListProjects,
		},
		{
			Name:        "get_project",
			Description: "Get a single project by ID. Returns full project details including directory and context files.",
			Mutating:    false,
			Fn:          toolGetProject,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Project ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_project",
			Description: "Create a new project. Requires title. Optionally accepts goals, project_dir.",
			Mutating:    true,
			Fn:          toolCreateProject,
			Properties:  map[string]PropertySchema{"title": {Type: "string", Description: "Project title"}, "goals": {Type: "string", Description: "Project goals (markdown)"}, "project_dir": {Type: "string", Description: "Project directory path"}},
			Required:    []string{"title"},
		},
		{
			Name:        "update_project",
			Description: "Update an existing project's fields (title, goals, project_dir).",
			Mutating:    true,
			Fn:          toolUpdateProject,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Project ID"}, "title": {Type: "string", Description: "New title"}, "goals": {Type: "string", Description: "New goals"}, "project_dir": {Type: "string", Description: "New project directory"}},
			Required:    []string{"id"},
		},
		{
			Name:        "delete_project",
			Description: "Delete a project by ID. This is irreversible.",
			Mutating:    true,
			Fn:          toolDeleteProject,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Project ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_project_directory",
			Description: "Create a project directory on the filesystem and set it as the project's project_dir. Optionally scaffold subdirectories (src, docs, tests, workflows).",
			Mutating:    true,
			Fn:          toolCreateProjectDir,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Project ID"}, "scaffold": {Type: "boolean", Description: "Whether to create scaffold subdirectories"}},
			Required:    []string{"project_id"},
		},
		{
			Name:        "archive_project",
			Description: "Archive a project by ID. The project is marked archived and excluded from active work.",
			Mutating:    true,
			Fn:          toolArchiveProject,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Project ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "list_project_dir",
			Description: "List the top-level entries of a project's project_dir (or a subdirectory of it). Read-only and path-traversal-safe: paths resolve inside the project directory only — use it to see what is in a project, existing or one just created in-conversation.",
			Mutating:    false,
			Fn:          toolListProjectDir,
			Properties: map[string]PropertySchema{
				"project_id": {Type: "string", Description: "Project ID"},
				"path":       {Type: "string", Description: "Optional path inside the project directory — a relative subpath or an absolute path within it. Defaults to the project_dir root."},
			},
			Required: []string{"project_id"},
		},
		{
			Name:        "read_project_file",
			Description: "Read a file inside a project's project_dir (or a subdirectory of it). Read-only and path-traversal-safe: paths resolve inside the project directory only — use it to read files in a project, existing or one just created in-conversation.",
			Mutating:    false,
			Fn:          toolReadProjectFile,
			Properties: map[string]PropertySchema{
				"project_id": {Type: "string", Description: "Project ID"},
				"path":       {Type: "string", Description: "File path — a relative path inside the project directory or an absolute path within it"},
				"max_bytes":  {Type: "number", Description: "Optional max bytes to read (1 to 262144; default 262144)"},
			},
			Required: []string{"project_id", "path"},
		},

		// --- Work Items ---
		{
			Name:        "list_work_items",
			Description: "List work items for a project or tenant. Supports filter by status, kind, search.",
			Mutating:    false,
			Fn:          toolListWorkItems,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}, "kind": {Type: "string", Description: "Optional kind filter"}},
		},
		{
			Name:        "get_work_item",
			Description: "Get a single work item by ID.",
			Mutating:    false,
			Fn:          toolGetWorkItem,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_work_item",
			Description: "Create a new work item within a project. Requires title and project_id. Optionally accepts kind, parent_id, description, acceptance_criteria, priority, budgets, context_window, workflow_id, scheduled_start_at, auto_start_workflow, runtime_image, context_files.",
			Mutating:    true,
			Fn:          toolCreateWorkItem,
			Properties: map[string]PropertySchema{
				"title": {Type: "string", Description: "Work item title"},
				"project_id": {Type: "string", Description: "Project ID"},
				"parent_id": {Type: "string", Description: "Optional parent work item ID"},
				"kind": {Type: "string", Description: "Work item kind (epic, feature, task, subtask)"},
				"description": {Type: "string", Description: "Detailed description (markdown)"},
				"acceptance_criteria": {Type: "string", Description: "Acceptance criteria (markdown)"},
				"priority": {Type: "number", Description: "Priority (1-5)"},
				"budgets": {Type: "string", Description: "Budgets as a JSON object (e.g. {\"max_steps\": 10, \"max_cost_usd\": 5})"},
				"context_window": {Type: "number", Description: "Context window size for the run"},
				"workflow_id": {Type: "string", Description: "Workflow template ID to bind this item to (must be a published workflow in the project to run)"},
				"scheduled_start_at": {Type: "string", Description: "Scheduled start time (ISO 8601 or 'N minutes from now'). Setting this marks the item scheduled."},
				"auto_start_workflow": {Type: "boolean", Description: "Start the bound workflow immediately on save (opt-in, default false). Only applies when workflow_id is set and no scheduled_start_at is given; conflicts with a schedule."},
				"runtime_image": {Type: "string", Description: "Runtime container image tag; empty = base image"},
				"context_files": {Type: "array", Description: "Absolute file or directory paths to include as worker context (same model as project context files)"},
			},
			Required: []string{"title", "project_id"},
		},
		{
			Name:        "update_work_item",
			Description: "Update any mutable field on a work item by ID: title, description, acceptance_criteria, acceptance_review, status, priority, budgets, context_window, project_id, workflow_id, parent_id, scheduled_start_at, auto_start_workflow, workflow_run_id, runtime_image, context_files, kind. Switching kind (kind: epic|feature|task|subtask) automatically resolves the hierarchy: the parent walks up to the nearest ancestor shallower than the new kind, direct children that can no longer sit under the item move under its parent, and switching to a non-schedulable kind (epic/feature) clears the worker assignment and scheduled start and demotes ready/assigned/scheduled to pending. Switching an epic to another kind requires choosing a parent explicitly.",
			Mutating:    true,
			Fn:          toolUpdateWorkItem,
			Properties: map[string]PropertySchema{
				"id": {Type: "string", Description: "Work item ID"},
				"title": {Type: "string", Description: "New title"},
				"description": {Type: "string", Description: "New description (markdown)"},
				"acceptance_criteria": {Type: "string", Description: "New acceptance criteria (markdown)"},
				"acceptance_review": {Type: "string", Description: "New acceptance review (markdown); empty string clears it (auto-populated by the WorkflowReconciler when a bound run completes)"},
				"status": {Type: "string", Description: "New status (pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering)"},
				"priority": {Type: "number", Description: "New priority (1-5)"},
				"budgets": {Type: "string", Description: "Budgets as a JSON object (e.g. {\"max_steps\": 10, \"max_cost_usd\": 5})"},
				"context_window": {Type: "number", Description: "Context window size for the run"},
				"project_id": {Type: "string", Description: "Reassign to a different project (target must be active)"},
				"workflow_id": {Type: "string", Description: "Bind/unbind to a workflow template ID (empty string clears the binding)"},
				"parent_id": {Type: "string", Description: "New parent work item ID (reparent). Must be the same project and a strictly higher-level kind (epic > feature > task > subtask). Empty string clears the parent (epic only)."},
				"scheduled_start_at": {Type: "string", Description: "Scheduled start time (ISO 8601 or 'N minutes from now'). Setting this flips the item to scheduled unless it has an active run."},
				"auto_start_workflow": {Type: "boolean", Description: "Start the bound workflow immediately on save. true with no scheduled_start_at clears any existing schedule."},
				"workflow_run_id": {Type: "string", Description: "The workflow run ID this item is bound to; empty string allows re-scheduling"},
				"runtime_image": {Type: "string", Description: "Runtime container image tag; empty string resets to the base image"},
				"context_files": {Type: "array", Description: "Absolute file or directory paths to include as worker context (same model as project context files); an empty list clears the selection"},
				"kind": {Type: "string", Description: "New kind (epic, feature, task, subtask). The parent/child hierarchy is resolved automatically (see description)."},
			},
			Required: []string{"id"},
		},
		{
			Name:        "assign_worker",
			Description: "Assign a worker to a work item. Requires the work item ID and the worker's ID and version (from list_workers / get_worker).",
			Mutating:    true,
			Fn:          toolAssignWorker,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}, "worker_id": {Type: "string", Description: "Worker ID"}, "version": {Type: "number", Description: "Worker version"}},
			Required:    []string{"id", "worker_id"},
		},
		{
			Name:        "unassign_worker",
			Description: "Remove the worker binding from a work item.",
			Mutating:    true,
			Fn:          toolUnassignWorker,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "schedule_work_item",
			Description: "Schedule a work item: set its status to scheduled and optionally set a start time. Provide id and optionally scheduled_time (ISO 8601 or 'N minutes from now').",
			Mutating:    true,
			Fn:          toolScheduleWorkItem,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}, "scheduled_time": {Type: "string", Description: "Optional scheduled time (ISO 8601 or 'N minutes from now')"}},
			Required:    []string{"id"},
		},
		{
			Name:        "reorder_work_items",
			Description: "Reorder the direct children of a work item (or top-level items when parent_id is empty) within a project — the sequence chain order. Provide project_id, an optional parent_id, and child_ids in the new order. Only this reorders a sequence; display sort never mutates it.",
			Mutating:    true,
			Fn:          toolReorderWorkItems,
			Properties: map[string]PropertySchema{
				"project_id": {Type: "string", Description: "Project ID"},
				"parent_id":  {Type: "string", Description: "Parent work item ID (empty = top level)"},
				"child_ids":  {Type: "array", Description: "Direct child work item IDs in the new order"},
			},
			Required: []string{"project_id", "child_ids"},
		},
		{
			Name:        "control_sequence",
			Description: "Drive a sequence parent manually (a work item that has children IS a sequence run). Provide id and action: start (re-fires the chain from child #1 — destructive, wipes prior child successes), resume (continues from the first non-succeeded child, keeps state; enabled when halted/failed or parked/pending), or stop (parks the chain: parent → pending and clears the scheduled start, so children can be run standalone; an in-flight child finishes naturally).",
			Mutating:    true,
			Fn:          toolControlSequence,
			Properties: map[string]PropertySchema{
				"id":     {Type: "string", Description: "Work item ID (the sequence parent)"},
				"action": {Type: "string", Description: "start, resume, or stop"},
			},
			Required: []string{"id", "action"},
		},
		{
			Name:        "delete_work_item",
			Description: "Soft-delete a work item by ID (status → cancelled). This is reversible via update_work_item.",
			Mutating:    true,
			Fn:          toolDeleteWorkItem,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}},
			Required:    []string{"id"},
		},

		// --- Workers ---
		{
			Name:        "list_workers",
			Description: "List all workers for the current tenant.",
			Mutating:    false,
			Fn:          toolListWorkers,
		},
		{
			Name:        "get_worker",
			Description: "Get a single worker by ID, including its latest version details.",
			Mutating:    false,
			Fn:          toolGetWorker,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Worker ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_worker",
			Description: "Create a new worker with a name and optional runtime/model configuration.",
			Mutating:    true,
			Fn:          toolCreateWorker,
			Properties:  map[string]PropertySchema{"name": {Type: "string", Description: "Worker name"}, "purpose": {Type: "string", Description: "Worker purpose"}, "runtime_ref": {Type: "string", Description: "Runtime reference (e.g. opencode)"}, "model_ref": {Type: "string", Description: "Model reference (e.g. opencode-go/deepseek-v4-flash)"}},
			Required:    []string{"name"},
		},
		{
			Name:        "update_worker",
			Description: "Update a worker's header fields (name, purpose).",
			Mutating:    true,
			Fn:          toolUpdateWorker,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Worker ID"}, "name": {Type: "string", Description: "New name"}, "purpose": {Type: "string", Description: "New purpose"}},
			Required:    []string{"id"},
		},
		{
			Name:        "delete_worker",
			Description: "Delete a worker by ID and all of its versions. This is irreversible.",
			Mutating:    true,
			Fn:          toolDeleteWorker,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Worker ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "list_worker_versions",
			Description: "List all versions of a worker by worker_id, with their status (draft/published/deprecated).",
			Mutating:    false,
			Fn:          toolListWorkerVersions,
			Properties:  map[string]PropertySchema{"worker_id": {Type: "string", Description: "Worker ID"}},
			Required:    []string{"worker_id"},
		},
		{
			Name:        "publish_worker_version",
			Description: "Publish a draft worker version, making it immutable and dispatchable. Provide worker_id and the version number.",
			Mutating:    true,
			Fn:          toolPublishWorkerVersion,
			Properties:  map[string]PropertySchema{"worker_id": {Type: "string", Description: "Worker ID"}, "version": {Type: "number", Description: "Version number to publish"}},
			Required:    []string{"worker_id", "version"},
		},
		{
			Name:        "deprecate_worker_version",
			Description: "Deprecate a published worker version. Provide worker_id and the version number.",
			Mutating:    true,
			Fn:          toolDeprecateWorkerVersion,
			Properties:  map[string]PropertySchema{"worker_id": {Type: "string", Description: "Worker ID"}, "version": {Type: "number", Description: "Version number to deprecate"}},
			Required:    []string{"worker_id", "version"},
		},
		{
			Name:        "set_active_worker_version",
			Description: "Set the active (default) version of a worker. Provide worker_id and a published version number.",
			Mutating:    true,
			Fn:          toolSetActiveWorkerVersion,
			Properties:  map[string]PropertySchema{"worker_id": {Type: "string", Description: "Worker ID"}, "version": {Type: "number", Description: "Published version number to make active"}},
			Required:    []string{"worker_id", "version"},
		},

		// --- Workflows ---
		{
			Name:        "list_workflows",
			Description: "List all workflows for the current tenant.",
			Mutating:    false,
			Fn:          toolListWorkflows,
		},
		{
			Name:        "get_workflow",
			Description: "Get a single workflow by ID.",
			Mutating:    false,
			Fn:          toolGetWorkflow,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Workflow ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "get_workflow_version",
			Description: "Get a single workflow version by workflow_id and optional version number (defaults to the latest published). Returns the steps JSON array verbatim — use it to adopt a UI-built workflow configuration as the seed template (bake the steps into internal/db/seed_workflows.go).",
			Mutating:    false,
			Fn:          toolGetWorkflowVersion,
			Properties:  map[string]PropertySchema{
				"workflow_id": {Type: "string", Description: "Workflow ID"},
				"version":     {Type: "number", Description: "Optional version number (defaults to latest published)"},
			},
			Required: []string{"workflow_id"},
		},
		{
			Name:        "create_workflow",
			Description: "Create a new workflow with a name and optional template.",
			Mutating:    true,
			Fn:          toolCreateWorkflow,
			Properties:  map[string]PropertySchema{"name": {Type: "string", Description: "Workflow name"}},
			Required:    []string{"name"},
		},

		// --- Workflow Runs ---
		{
			Name:        "list_workflow_runs",
			Description: "List workflow runs, optionally filtered by workflow_id or status.",
			Mutating:    false,
			Fn:          toolListWorkflowRuns,
			Properties:  map[string]PropertySchema{"workflow_id": {Type: "string", Description: "Optional workflow ID filter"}, "status": {Type: "string", Description: "Optional status filter"}},
		},
		{
			Name:        "get_workflow_run",
			Description: "Get a single workflow run by ID, including its current step and status.",
			Mutating:    false,
			Fn:          toolGetWorkflowRun,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Workflow run ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "force_progress_workflow_run",
			Description: "Force a stuck running workflow run past its current in-flight step run(s), regardless of their previous status. Use when a run is wedged 'running' even though the worker execution succeeded (e.g. a reconcile error on a later step rolled back an upstream step's terminal mark). Marks active step runs succeeded and lets the reconciler advance the DAG.",
			Mutating:    true,
			Fn:          toolForceProgressWorkflowRun,
			Properties:  map[string]PropertySchema{"run_id": {Type: "string", Description: "Workflow run ID to force-progress"}},
			Required:    []string{"run_id"},
		},
		{
			Name:        "retry_failed_workflow_run",
			Description: "Resume a FAILED workflow run in place: reset the run back to pending, re-arm the failed/skipped/blocked step runs for re-dispatch, and flip the bound work item back to running. Steps that already succeeded are kept — the DAG resumes from where it left off. Only works on a failed run.",
			Mutating:    true,
			Fn:          toolRetryFailedWorkflowRun,
			Properties:  map[string]PropertySchema{"run_id": {Type: "string", Description: "Workflow run ID of the failed run to retry"}},
			Required:    []string{"run_id"},
		},

		// --- Executions ---
		{
			Name:        "list_executions",
			Description: "List executions, optionally filtered by project, status, or workflow run.",
			Mutating:    false,
			Fn:          toolListExecutions,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}},
		},
		{
			Name:        "get_execution",
			Description: "Get a single execution by ID, including conversation, output, and error details.",
			Mutating:    false,
			Fn:          toolGetExecution,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Execution ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "cancel_execution",
			Description: "Cancel a running execution by ID. Provide a reason.",
			Mutating:    true,
			Fn:          toolCancelExecution,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Execution ID"}, "reason": {Type: "string", Description: "Reason for cancellation"}},
			Required:    []string{"id", "reason"},
		},

		// --- Telemetry / Diagnostics ---
		{
			Name:        "diagnose_failure",
			Description: "Diagnose why a workflow or execution failed. Provide the execution ID or workflow run ID to get failure analysis.",
			Mutating:    false,
			Fn:          toolDiagnoseFailure,
			Properties:  map[string]PropertySchema{"execution_id": {Type: "string", Description: "Execution ID to diagnose"}, "workflow_run_id": {Type: "string", Description: "Workflow run ID to diagnose"}},
		},
		{
			Name:        "get_usage",
			Description: "Get token usage and cost data. Optionally filter by project.",
			Mutating:    false,
			Fn:          toolGetUsage,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}},
		},

		// --- Policies ---
		{
			Name:        "list_policies",
			Description: "List all policies for the current tenant, optionally filtered by status.",
			Mutating:    false,
			Fn:          toolListPolicies,
			Properties:  map[string]PropertySchema{"status": {Type: "string", Description: "Optional status filter (draft/published)"}},
		},
		{
			Name:        "get_policy",
			Description: "Get a single policy by ID.",
			Mutating:    false,
			Fn:          toolGetPolicy,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Policy ID"}},
			Required:    []string{"id"},
		},

		// --- Recoveries ---
		{
			Name:        "list_recoveries",
			Description: "List recovery executions, optionally filtered by project or status.",
			Mutating:    false,
			Fn:          toolListRecoveries,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}},
		},

		// --- Runtime Images ---
		{
			Name:        "list_runtime_images",
			Description: "List all runtime images for the current tenant (container images workers run in).",
			Mutating:    false,
			Fn:          toolListRuntimeImages,
		},
		{
			Name:        "get_runtime_image",
			Description: "Get a single runtime image by ID, including its build status and spec.",
			Mutating:    false,
			Fn:          toolGetRuntimeImage,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Runtime image ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_runtime_image",
			Description: "Create a new runtime image spec (draft status) with a name, slug, and optional apt packages + toolchain lines. The image is built later via the UI.",
			Mutating:    true,
			Fn:          toolCreateRuntimeImage,
			Properties:  map[string]PropertySchema{"name": {Type: "string", Description: "Image name"}, "slug": {Type: "string", Description: "Image slug (lowercase words with hyphens)"}, "description": {Type: "string", Description: "Optional description"}, "apt_packages": {Type: "array", Description: "Optional list of apt package names"}, "toolchains": {Type: "array", Description: "Optional toolchain install lines (pip/npm/mise/curl)"}},
			Required:    []string{"name", "slug"},
		},
		{
			Name:        "delete_runtime_image",
			Description: "Delete a runtime image by ID and its local Docker image. This is irreversible.",
			Mutating:    true,
			Fn:          toolDeleteRuntimeImage,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Runtime image ID"}},
			Required:    []string{"id"},
		},

		// --- Settings ---
		{
			Name:        "get_settings",
			Description: "Get the current tenant settings (default models, stall parameters).",
			Mutating:    false,
			Fn:          toolGetSettings,
		},		{
			Name:        "update_settings",
			Description: "Update tenant settings (default models, stall parameters). All fields are optional — zero/empty fields are left unchanged.",
			Mutating:    true,
			Fn:          toolUpdateSettings,
			Properties: map[string]PropertySchema{
				"default_worker_model":              {Type: "string", Description: "Default model for workers"},
				"default_ask_orchicon_model":        {Type: "string", Description: "Default model for Ask Orchicon"},
				"stall_no_progress_window_seconds":  {Type: "number", Description: "Seconds without token progress before a session is considered stalled (0 = leave unchanged)"},
				"stall_no_file_diff_window_seconds": {Type: "number", Description: "Seconds without file modifications before an advisory liveness probe is sent (0 = leave unchanged)"},
				"stall_text_loop_window_seconds":    {Type: "number", Description: "Seconds window for text-loop detection (0 = leave unchanged)"},
				"stall_repetition_count":            {Type: "number", Description: "Same tool-call signature repeated this many times within the repetition window before aborting (0 = leave unchanged)"},
				"stall_repetition_window_seconds":   {Type: "number", Description: "Seconds window for repetition detection (0 = leave unchanged)"},
			},
		},

		// --- Audit ---
		{
			Name:        "list_audit_events",
			Description: "List audit events for the current tenant — the actor-based 'who did what' trail (action, actor, auth method, target, before/after, trace id). Optional filters: action, actor_id, target_type, target_id. Read-only.",
			Mutating:    false,
			Fn:          toolListAuditEvents,
			Properties: map[string]PropertySchema{
				"action":      {Type: "string", Description: "Optional exact action filter (e.g. work_item.created)"},
				"actor_id":    {Type: "string", Description: "Optional actor identity ID filter"},
				"target_type": {Type: "string", Description: "Optional target type filter (e.g. work_item)"},
				"target_id":   {Type: "string", Description: "Optional target ID filter"},
				"page_size":   {Type: "number", Description: "Optional page size (max 1000, default 100)"},
			},
		},
	}
}
