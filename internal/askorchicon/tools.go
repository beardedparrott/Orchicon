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
			Description: "Create a new work item within a project. Requires title and project_id. Optionally accepts parent_id, kind, description, acceptance_criteria, priority.",
			Mutating:    true,
			Fn:          toolCreateWorkItem,
			Properties: map[string]PropertySchema{
				"title": {Type: "string", Description: "Work item title"},
				"project_id": {Type: "string", Description: "Project ID"},
				"parent_id": {Type: "string", Description: "Optional parent work item ID"},
				"kind": {Type: "string", Description: "Work item kind (Epic, Feature, Task, Subtask)"},
				"description": {Type: "string", Description: "Detailed description"},
				"acceptance_criteria": {Type: "string", Description: "Acceptance criteria"},
				"priority": {Type: "number", Description: "Priority (1-5)"},
			},
			Required: []string{"title", "project_id"},
		},
		{
			Name:        "update_work_item",
			Description: "Update a work item's fields (title, description, status, priority, etc.).",
			Mutating:    true,
			Fn:          toolUpdateWorkItem,
			Properties: map[string]PropertySchema{
				"id": {Type: "string", Description: "Work item ID"},
				"title": {Type: "string", Description: "New title"},
				"description": {Type: "string", Description: "New description"},
				"status": {Type: "string", Description: "New status (draft, ready, assigned, running, done, failed, cancelled)"},
				"priority": {Type: "number", Description: "New priority (1-5)"},
			},
			Required: []string{"id"},
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

		// --- Settings ---
		{
			Name:        "get_settings",
			Description: "Get the current tenant settings (default models, stall parameters).",
			Mutating:    false,
			Fn:          toolGetSettings,
		},
		{
			Name:        "update_settings",
			Description: "Update tenant settings (default models, stall parameters).",
			Mutating:    true,
			Fn:          toolUpdateSettings,
			Properties:  map[string]PropertySchema{"default_worker_model": {Type: "string", Description: "Default model for workers"}, "default_ask_orchicon_model": {Type: "string", Description: "Default model for Ask Orchicon"}},
		},
	}
}
