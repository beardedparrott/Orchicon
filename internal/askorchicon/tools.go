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
	tools  []ToolDefinition
	byName map[string]ToolDefinition
}

// NewToolRegistry creates the registry with all available tools.
func NewToolRegistry(pool *db.Pool, log *slog.Logger, secretsKEK []byte) *ToolRegistry {
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
	for _, td := range allTools(pool, log, secretsKEK) {
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
func allTools(pool *db.Pool, log *slog.Logger, secretsKEK []byte) []ToolDefinition {
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
			Description: "List work items for a project or tenant. Supports filter by status, kind, search. Returns a bounded, compact list ({count, truncated, note, items}) — branch to get_work_item for full detail, or pass next_page_token to page through the rest.",
			Mutating:    false,
			Fn:          toolListWorkItems,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}, "kind": {Type: "string", Description: "Optional kind filter"}, "search": {Type: "string", Description: "Free-text search across title and description"}, "page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"}},
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
			Name:        "get_work_item_run_history",
			Description: "Get a recurring work item's per-fire run history (each fire's status, fire time, bound workflow run, and that run's executions + outputs). Returns an empty array for an item that has never fired.",
			Mutating:    false,
			Fn:          toolGetWorkItemRunHistory,
			Properties:  map[string]PropertySchema{"work_item_id": {Type: "string", Description: "Recurring work item ID"}},
			Required:    []string{"work_item_id"},
		},
		{
			Name:        "list_ideas",
			Description: "List the Idea Cloud (feature 5.1): idea-state work items with their automation provenance (spawned_by + spawned_by_run_id) and a read-time SpawnedByTitle badge. Idea-state items are system-managed and excluded from the normal Work Items scope; they only become queryable there via promote_idea. Set state=\"rejected\" to read the REJECTED section instead: previously dismissed idea spawns (durable rejection history — also what the automation dedupe gate checks before spawning). Returns a bounded, compact list ({count, truncated, note, items}) — branch to get_work_item for full detail, or pass next_page_token to page through the rest.",
			Mutating:    false,
			Fn:          toolListIdeas,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "search": {Type: "string", Description: "Free-text search across title and description"}, "state": {Type: "string", Description: "Optional idea population: \"active\" (default) = idea-state items awaiting triage; \"rejected\" = previously dismissed idea spawns"}, "sort_by": {Type: "string", Description: "Optional sort field: title, priority, created_at"}, "sort_order": {Type: "string", Description: "Optional sort order: asc or desc"}, "page_token": {Type: "string", Description: "Optional pagination token (id > cursor)"}, "page_size": {Type: "number", Description: "Optional page size"}},
		},
		{
			Name:        "promote_idea",
			Description: "Approve an idea (feature 5.1): transition an idea-state work item to a normal pending work item (leaves idea state, becomes queryable in the normal Work Items scope with normal status semantics, and can be planned/scheduled/run through the existing pipeline). Provenance is retained for display. Audited as work_item.promoted. PromoteIdea is the ONLY sanctioned path out of idea state.",
			Mutating:    true,
			Fn:          toolPromoteIdea,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Idea-state work item ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "dismiss_idea",
			Description: "Discard an idea (feature 5.1): transition an idea-state work item to cancelled (the soft-delete/cancel terminal, consistent with delete_work_item) so it leaves idea state and drops out of every active view. Provenance is retained as a record of where it came from. Audited as work_item.dismissed.",
			Mutating:    true,
			Fn:          toolDismissIdea,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Idea-state work item ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_work_item",
			Description: "Create a new work item within a project. Requires title and project_id. Optionally accepts kind, parent_id, description, acceptance_criteria, priority, budgets, context_window, workflow_id, scheduled_start_at, auto_start_workflow, runtime_image, context_files.",
			Mutating:    true,
			Fn:          toolCreateWorkItem,
			Properties: map[string]PropertySchema{
				"title":               {Type: "string", Description: "Work item title"},
				"project_id":          {Type: "string", Description: "Project ID"},
				"parent_id":           {Type: "string", Description: "Optional parent work item ID"},
				"kind":                {Type: "string", Description: "Work item kind (epic, feature, task, subtask)"},
				"description":         {Type: "string", Description: "Detailed description (markdown)"},
				"acceptance_criteria": {Type: "string", Description: "Acceptance criteria (markdown)"},
				"priority":            {Type: "number", Description: "Priority (1-5)"},
				"budgets":             {Type: "string", Description: "Budgets as a JSON object (e.g. {\"tool_call_count\": 100, \"max_cost_usd\": 5})"},
				"context_window":      {Type: "number", Description: "Context window size for the run"},
				"workflow_id":         {Type: "string", Description: "Workflow template ID to bind this item to (must be a published workflow in the project to run)"},
				"scheduled_start_at":  {Type: "string", Description: "Scheduled start time (ISO 8601 or 'N minutes from now'). Setting this marks the item scheduled."},
				"auto_start_workflow": {Type: "boolean", Description: "Start the bound workflow immediately on save (opt-in, default false). Only applies when workflow_id is set and no scheduled_start_at is given; conflicts with a schedule."},
				"runtime_image":       {Type: "string", Description: "Runtime container image tag; empty = base image"},
				"context_files":       {Type: "array", Description: "Absolute file or directory paths to include as worker context (same model as project context files)"},
			},
			Required: []string{"title", "project_id"},
		},
		{
			Name:        "update_work_item",
			Description: "Update any mutable field on a work item by ID: title, description, acceptance_criteria, acceptance_review, status, priority, budgets, context_window, project_id, workflow_id, parent_id, scheduled_start_at, auto_start_workflow, workflow_run_id, runtime_image, context_files, kind. Switching kind (kind: epic|feature|task|subtask) automatically resolves the hierarchy: the parent walks up to the nearest ancestor shallower than the new kind, direct children that can no longer sit under the item move under its parent, and switching to a non-schedulable kind (epic/feature) clears the worker assignment and scheduled start and demotes ready/assigned/scheduled to pending. Switching an epic to another kind requires choosing a parent explicitly.",
			Mutating:    true,
			Fn:          toolUpdateWorkItem,
			Properties: map[string]PropertySchema{
				"id":                  {Type: "string", Description: "Work item ID"},
				"title":               {Type: "string", Description: "New title"},
				"description":         {Type: "string", Description: "New description (markdown)"},
				"acceptance_criteria": {Type: "string", Description: "New acceptance criteria (markdown)"},
				"acceptance_review":   {Type: "string", Description: "New acceptance review (markdown); empty string clears it (auto-populated by the WorkflowReconciler when a bound run completes)"},
				"status":              {Type: "string", Description: "New status (pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering)"},
				"priority":            {Type: "number", Description: "New priority (1-5)"},
				"budgets":             {Type: "string", Description: "Budgets as a JSON object (e.g. {\"tool_call_count\": 100, \"max_cost_usd\": 5})"},
				"context_window":      {Type: "number", Description: "Context window size for the run"},
				"project_id":          {Type: "string", Description: "Reassign to a different project (target must be active)"},
				"workflow_id":         {Type: "string", Description: "Bind/unbind to a workflow template ID (empty string clears the binding)"},
				"parent_id":           {Type: "string", Description: "New parent work item ID (reparent). Must be the same project and a strictly higher-level kind (epic > feature > task > subtask). Empty string clears the parent (epic only)."},
				"scheduled_start_at":  {Type: "string", Description: "Scheduled start time (ISO 8601 or 'N minutes from now'). Setting this flips the item to scheduled unless it has an active run."},
				"auto_start_workflow": {Type: "boolean", Description: "Start the bound workflow immediately on save. true with no scheduled_start_at clears any existing schedule."},
				"workflow_run_id":     {Type: "string", Description: "The workflow run ID this item is bound to; empty string allows re-scheduling"},
				"runtime_image":       {Type: "string", Description: "Runtime container image tag; empty string resets to the base image"},
				"context_files":       {Type: "array", Description: "Absolute file or directory paths to include as worker context (same model as project context files); an empty list clears the selection"},
				"kind":                {Type: "string", Description: "New kind (epic, feature, task, subtask). The parent/child hierarchy is resolved automatically (see description)."},
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
		{
			Name:        "archive_work_item",
			Description: "Archive a terminal work item (succeeded/failed/cancelled/skipped) by ID — hides it from every normal work-item view (board/tree/list/sequence/workflows/counts). Only allowed from a terminal state and blocked when the item has children (archive the children first). Reversible via restore_work_item.",
			Mutating:    true,
			Fn:          toolArchiveWorkItem,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Work item ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "restore_work_item",
			Description: "Restore an archived work item by ID back to the active views, with the terminal status it was archived from. Reverses archive_work_item.",
			Mutating:    true,
			Fn:          toolRestoreWorkItem,
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
			Description: "Create a new worker AND its first draft version (v1) in one transaction — the version persists model_ref and the prompt fields, so the worker is immediately editable and publishable from the UI. Returns the worker row plus version and version_id.",
			Mutating:    true,
			Fn:          toolCreateWorker,
			Properties: map[string]PropertySchema{
				"name":          {Type: "string", Description: "Worker name"},
				"purpose":       {Type: "string", Description: "Worker purpose"},
				"model_ref":     {Type: "string", Description: "Model reference (adapter/provider/model, e.g. opencode/opencode-go/deepseek-v4-flash, or the legacy provider/model e.g. opencode-go/deepseek-v4-flash). Segment 1 selects and routes the per-worker adapter (ADR-0005): fresh selections default to the orchicon adapter (e.g. orchicon/commandcode/deepseek/deepseek-v4-flash); legacy 2-segment refs (provider/model) infer and keep dispatching to opencode — existing workers are never repointed. The ref's adapter segment is the single source of truth for dispatch; there is no separate runtime_ref."},
				"description":   {Type: "string", Description: "Optional human-readable description"},
				"version_note":  {Type: "string", Description: "Optional note describing draft version 1"},
				"role":          {Type: "string", Description: "Optional role section for the composed system prompt"},
				"skills":        {Type: "string", Description: "Optional skills section for the composed system prompt"},
				"behavior":      {Type: "string", Description: "Optional behavior section for the composed system prompt"},
				"agents_md":     {Type: "string", Description: "Optional AGENTS.md section for the composed system prompt"},
				"system_prompt": {Type: "string", Description: "Raw system prompt (used only when no role/skills/behavior/agents_md is provided)"},
			},
			Required: []string{"name"},
		},
		{
			Name:        "update_worker",
			Description: "Update a worker's header fields (name, description, purpose). Writes a worker.updated audit entry.",
			Mutating:    true,
			Fn:          toolUpdateWorker,
			Properties: map[string]PropertySchema{
				"id":          {Type: "string", Description: "Worker ID"},
				"name":        {Type: "string", Description: "New name"},
				"description": {Type: "string", Description: "New description"},
				"purpose":     {Type: "string", Description: "New purpose"},
			},
			Required: []string{"id"},
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
			Properties: map[string]PropertySchema{
				"workflow_id": {Type: "string", Description: "Workflow ID"},
				"version":     {Type: "number", Description: "Optional version number (defaults to latest published)"},
			},
			Required: []string{"workflow_id"},
		},
		{
			Name:        "create_workflow",
			Description: "Create a new workflow AND its first draft version (v1) in one transaction, seeding steps when provided — the workflow is immediately editable and publishable from the UI. Type defaults to template (no project_id) or one_shot (with project_id). description seeds the version-1 note when version_note is empty. Returns the workflow row plus version and version_id.",
			Mutating:    true,
			Fn:          toolCreateWorkflow,
			Properties: map[string]PropertySchema{
				"name":         {Type: "string", Description: "Workflow name"},
				"steps":        {Type: "array", Description: "Optional JSON array of step objects ({id, name, kind, ref, worker_version, depends_on, config, position_x, position_y}) seeded as draft version 1"},
				"description":  {Type: "string", Description: "Optional description; stored as the draft version-1 version_note when version_note is empty"},
				"version_note": {Type: "string", Description: "Optional note describing draft version 1"},
				"type":         {Type: "string", Description: "Optional workflow type: one_shot or template"},
				"git_strategy": {Type: "string", Description: "Optional git strategy: local, pr, or none"},
				"inputs":       {Type: "object", Description: "Optional JSON object of run inputs"},
				"outputs":      {Type: "object", Description: "Optional JSON object of run outputs"},
				"project_id":   {Type: "string", Description: "Optional project ID for a project-scoped one_shot workflow (project must be active)"},
			},
			Required: []string{"name"},
		},

		// --- Workflow Runs ---
		{
			Name:        "list_workflow_runs",
			Description: "List workflow runs, optionally filtered by workflow_id, project_id, work_item_id, or status. Returns a bounded, compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest, or get_workflow_run for full detail.",
			Mutating:    false,
			Fn:          toolListWorkflowRuns,
			Properties:  map[string]PropertySchema{"workflow_id": {Type: "string", Description: "Optional workflow ID filter"}, "project_id": {Type: "string", Description: "Optional project ID filter"}, "work_item_id": {Type: "string", Description: "Optional work item ID filter"}, "status": {Type: "string", Description: "Optional status filter"}, "sort_by": {Type: "string", Description: "Optional sort field: id or started_at"}, "sort_order": {Type: "string", Description: "Optional sort order: asc or desc"}, "page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"}},
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
			Description: "List executions, optionally filtered by project, status, or workflow run. Returns a bounded, compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest, or get_execution for full detail.",
			Mutating:    false,
			Fn:          toolListExecutions,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}, "task_id": {Type: "string", Description: "Optional work item ID filter"}, "page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"}},
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
			Description: "Get token usage and cost data. Optionally filter by project, provider, or model. Returns a bounded, compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest.",
			Mutating:    false,
			Fn:          toolGetUsage,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "provider": {Type: "string", Description: "Optional provider filter"}, "model": {Type: "string", Description: "Optional model filter"}, "page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"}},
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
			Description: "List recovery executions, optionally filtered by project or status. Returns a bounded, compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest.",
			Mutating:    false,
			Fn:          toolListRecoveries,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Optional project ID filter"}, "status": {Type: "string", Description: "Optional status filter"}, "page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"}},
		},

		// --- Categories ---
		{
			Name:        "list_categories",
			Description: "List categories for a target type (worker|workflow|conversation) with assignments. Each target_type has its own independent set.",
			Mutating:    false,
			Fn:          toolListCategories,
			Properties:  map[string]PropertySchema{"target_type": {Type: "string", Description: "Target type: worker, workflow, or conversation"}},
			Required:    []string{"target_type"},
		},
		{
			Name:        "create_category",
			Description: "Create a category for a target type (worker|workflow|conversation). Target type is required and immutable.",
			Mutating:    true,
			Fn:          toolCreateCategory,
			Properties:  map[string]PropertySchema{"target_type": {Type: "string", Description: "Target type"}, "name": {Type: "string", Description: "Category name (1-64 chars)"}, "description": {Type: "string", Description: "Optional description (max 256)"}},
			Required:    []string{"target_type", "name"},
		},
		{
			Name:        "update_category",
			Description: "Update a category name/description. Target type is immutable.",
			Mutating:    true,
			Fn:          toolUpdateCategory,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Category ID"}, "name": {Type: "string", Description: "New name"}, "description": {Type: "string", Description: "New description"}},
			Required:    []string{"id"},
		},
		{
			Name:        "delete_category",
			Description: "Delete a category. Assignments are removed (items move to Uncategorized).",
			Mutating:    true,
			Fn:          toolDeleteCategory,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Category ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "assign_to_category",
			Description: "Assign an entity to a category. Validates target_type matches the category.",
			Mutating:    true,
			Fn:          toolAssignToCategory,
			Properties:  map[string]PropertySchema{"category_id": {Type: "string", Description: "Category ID"}, "entity_id": {Type: "string", Description: "Entity ID"}, "target_type": {Type: "string", Description: "Target type"}},
			Required:    []string{"category_id", "entity_id", "target_type"},
		},
		{
			Name:        "unassign_from_category",
			Description: "Remove an entity from its category (move to Uncategorized).",
			Mutating:    true,
			Fn:          toolUnassignFromCategory,
			Properties:  map[string]PropertySchema{"entity_id": {Type: "string", Description: "Entity ID"}, "target_type": {Type: "string", Description: "Target type"}},
			Required:    []string{"entity_id", "target_type"},
		},
		{
			Name:        "reorder_categories",
			Description: "Reorder categories within a target_type set. ordered_ids must be a permutation of existing ids.",
			Mutating:    true,
			Fn:          toolReorderCategories,
			Properties:  map[string]PropertySchema{"target_type": {Type: "string", Description: "Target type"}, "ordered_ids": {Type: "array", Description: "Category IDs in new order"}},
			Required:    []string{"target_type", "ordered_ids"},
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
			Description: "Create a new runtime image spec (draft status) with a name, slug, and optional apt packages + toolchain lines. The image is built later via the UI. Accepts optional env (JSON object string) and dockerfile_override.",
			Mutating:    true,
			Fn:          toolCreateRuntimeImage,
			Properties:  map[string]PropertySchema{"name": {Type: "string", Description: "Image name"}, "slug": {Type: "string", Description: "Image slug (lowercase words with hyphens)"}, "description": {Type: "string", Description: "Optional description"}, "apt_packages": {Type: "array", Description: "Optional list of apt package names"}, "toolchains": {Type: "array", Description: "Optional toolchain install lines (pip/npm/mise/curl)"}, "env": {Type: "string", Description: "Optional env as JSON object string (e.g. \"{\\\"PLAYWRIGHT_BROWSERS_PATH\\\":\\\"/ms-playwright\\\"}\")"}, "dockerfile_override": {Type: "string", Description: "Optional raw Dockerfile text (empty = generate from structured fields)"}, "tag": {Type: "string", Description: "Optional tag; defaults to \"<slug>:latest\""}},
			Required:    []string{"name", "slug"},
		},
		{
			Name:        "update_runtime_image",
			Description: "Update a runtime image spec (draft/failed; ready reverts to draft). Requires id + version (optimistic concurrency). Optionally updates name, slug, description, apt_packages, toolchains, env (JSON object string), dockerfile_override, tag.",
			Mutating:    true,
			Fn:          toolUpdateRuntimeImage,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Runtime image ID"}, "version": {Type: "number", Description: "Current spec version (optimistic concurrency)"}, "name": {Type: "string", Description: "New name"}, "slug": {Type: "string", Description: "New slug"}, "description": {Type: "string", Description: "New description"}, "apt_packages": {Type: "array", Description: "New apt packages"}, "toolchains": {Type: "array", Description: "New toolchain lines"}, "env": {Type: "string", Description: "New env as JSON object string"}, "dockerfile_override": {Type: "string", Description: "New Dockerfile override"}, "tag": {Type: "string", Description: "New tag"}},
			Required:    []string{"id", "version"},
		},
		{
			Name:        "build_runtime_image",
			Description: "Build a runtime image via the runtime daemon (docker build). Requires id + version (optimistic concurrency). Streams build logs and returns final status/tag/error/skipped plus aggregated logs. Skipped true when spec is already built.",
			Mutating:    true,
			Fn:          toolBuildRuntimeImage,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Runtime image ID"}, "version": {Type: "number", Description: "Current spec version (optimistic concurrency)"}},
			Required:    []string{"id", "version"},
		},
		{
			Name:        "delete_runtime_image",
			Description: "Delete a runtime image by ID and its local Docker image. This is irreversible.",
			Mutating:    true,
			Fn:          toolDeleteRuntimeImage,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "Runtime image ID"}},
			Required:    []string{"id"},
		},

		// --- Secrets ---
		{
			Name:        "list_secrets",
			Description: "List tenant secrets (encrypted at rest; values never returned).",
			Mutating:    false,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolListSecrets(ctx, pool, secretsKEK, args)
			},
		},
		{
			Name:        "create_secret",
			Description: "Create a tenant secret (AES-256-GCM encrypted at rest). Name must match ^[A-Z][A-Z0-9_]+$.",
			Mutating:    true,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolCreateSecret(ctx, pool, secretsKEK, args)
			},
			Properties: map[string]PropertySchema{"name": {Type: "string", Description: "Secret name (e.g. TAVILY_API_KEY)"}, "value": {Type: "string", Description: "Secret value (plaintext, encrypted at rest)"}, "description": {Type: "string", Description: "Optional description"}},
			Required:   []string{"name", "value"},
		},
		{
			Name:        "update_secret",
			Description: "Update a tenant secret value or description.",
			Mutating:    true,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolUpdateSecret(ctx, pool, secretsKEK, args)
			},
			Properties: map[string]PropertySchema{"id": {Type: "string", Description: "Secret ID"}, "value": {Type: "string", Description: "New secret value"}, "description": {Type: "string", Description: "New description"}},
			Required:   []string{"id"},
		},
		{
			Name:        "delete_secret",
			Description: "Delete a tenant secret by ID.",
			Mutating:    true,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolDeleteSecret(ctx, pool, secretsKEK, args)
			},
			Properties: map[string]PropertySchema{"id": {Type: "string", Description: "Secret ID"}},
			Required:   []string{"id"},
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
			Description: "Update tenant settings (default models, stall parameters, nudge, execution budget, reaper). All fields are optional — zero/empty fields are left unchanged.",
			Mutating:    true,
			Fn:          toolUpdateSettings,
			Properties: map[string]PropertySchema{
				"default_worker_model":                {Type: "string", Description: "Default model for workers"},
				"default_ask_orchicon_model":          {Type: "string", Description: "Default model for Ask Orchicon"},
				"stall_no_progress_window_seconds":    {Type: "number", Description: "Seconds without token progress before a session is considered stalled (0 = leave unchanged)"},
				"stall_no_file_diff_window_seconds":   {Type: "number", Description: "Seconds without file modifications before an advisory liveness probe is sent (0 = leave unchanged)"},
				"stall_text_loop_window_seconds":      {Type: "number", Description: "Seconds window for text-loop detection (0 = leave unchanged)"},
				"stall_repetition_count":              {Type: "number", Description: "Same tool-call signature repeated this many times within the repetition window before aborting (0 = leave unchanged)"},
				"stall_repetition_window_seconds":     {Type: "number", Description: "Seconds window for repetition detection (0 = leave unchanged)"},
				"stall_nudge_max":                     {Type: "number", Description: "Max liveness probes sent before an advisory stall escalates to fatal (0 = leave unchanged)"},
				"stall_nudge_reply_window_seconds":    {Type: "number", Description: "Seconds a probe is awaited before the execution is considered unresponsive (0 = leave unchanged)"},
				"stall_nudge_cooldown_seconds":        {Type: "number", Description: "Seconds between consecutive probes (0 = leave unchanged)"},
				"stall_tool_hang_seconds":             {Type: "number", Description: "Seconds a tool call with no events is allowed before it is cancelled natively and a course-correcting redirect is injected (0 = leave unchanged; negative = disabled)"},
				"default_budget_overrides":            {Type: "string", Description: "JSON object of default execution-budget gates (e.g. {\"tokens\":500000,\"cost_usd\":0.5,\"wall_clock_seconds\":7200,\"tool_call_count\":0,\"compact_max_turns\":12}). Empty string = leave unchanged."},
				"execution_reap_grace_seconds":        {Type: "number", Description: "Liveness reaper grace before a stuck running execution is reaped (0 = leave unchanged)"},
				"execution_reap_consecutive_failures": {Type: "number", Description: "Liveness probe failures before the reaper acts (0 = leave unchanged)"},
			},
		},

		// --- MCP servers (adapter-settings MCP management) ---
		{
			Name:        "list_mcp_servers",
			Description: "List MCP server entries for the current tenant (Settings → Adapters → MCP). Credentials never appear — env/header values are ${SECRET_NAME} references; has_secret_stored reports whether any required secret exists.",
			Mutating:    false,
			Fn:          toolListMCPServers,
		},
		{
			Name:        "get_mcp_server",
			Description: "Get a single MCP server entry by ID (Settings → Adapters → MCP). Credentials never appear — env/header values are ${SECRET_NAME} references.",
			Mutating:    false,
			Fn:          toolGetMCPServer,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "MCP server ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "create_mcp_server",
			Description: "Create an MCP server entry (tenant-scoped). Transport is 'stdio' (command + args + env) or 'streamable-http' (url + headers). Catalog entries are one-click added via list_mcp_catalog + create_mcp_server with catalog_slug.",
			Mutating:    true,
			Fn:          toolCreateMCPServer,
			Properties: map[string]PropertySchema{
				"name":         {Type: "string", Description: "Entry name (immutable after create)"},
				"transport":    {Type: "string", Description: "'stdio' or 'streamable-http' (default stdio)"},
				"command":      {Type: "string", Description: "stdio: executable"},
				"args":         {Type: "array", Description: "stdio: argv array"},
				"env":          {Type: "object", Description: "stdio: env map; values may be ${SECRET_NAME} references"},
				"url":          {Type: "string", Description: "streamable-http: endpoint URL"},
				"headers":      {Type: "object", Description: "streamable-http: headers; values may be ${SECRET_NAME} references"},
				"enabled":      {Type: "boolean", Description: "Enabled flag (default false)"},
				"catalog_slug": {Type: "string", Description: "Registry provenance slug, e.g. 'github'"},
			},
			Required: []string{"name"},
		},
		{
			Name:        "update_mcp_server",
			Description: "Update an MCP server entry (name is immutable). Partial update: pass only the fields to change; env/headers merge unless replace_env/replace_headers is true.",
			Mutating:    true,
			Fn:          toolUpdateMCPServer,
			Properties: map[string]PropertySchema{
				"id":              {Type: "string", Description: "MCP server ID"},
				"transport":       {Type: "string", Description: "'stdio' or 'streamable-http'"},
				"command":         {Type: "string", Description: "stdio: executable"},
				"args":            {Type: "array", Description: "stdio: argv array"},
				"replace_args":    {Type: "boolean", Description: "Replace args entirely (default merges nothing; args only replace when true)"},
				"env":             {Type: "object", Description: "stdio: env map; values may be ${SECRET_NAME}"},
				"replace_env":     {Type: "boolean", Description: "Replace env entirely instead of merging"},
				"url":             {Type: "string", Description: "streamable-http: endpoint URL"},
				"headers":         {Type: "object", Description: "streamable-http: headers"},
				"replace_headers": {Type: "boolean", Description: "Replace headers entirely instead of merging"},
				"enabled":         {Type: "boolean", Description: "Enabled flag"},
				"catalog_slug":    {Type: "string", Description: "Registry provenance slug"},
			},
			Required: []string{"id"},
		},
		{
			Name:        "delete_mcp_server",
			Description: "Delete an MCP server entry. Blocked while any project/worker/tenant-default set still references it — clear references first.",
			Mutating:    true,
			Fn:          toolDeleteMCPServer,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "MCP server ID"}},
			Required:    []string{"id"},
		},
		{
			Name:        "install_mcp_server",
			Description: "Explicit-only auto-install for an MCP server entry (catalog entries with an installable command). Detects the runtime (npx/uvx/docker) on the host, runs the install, records the result on the entry. dry_run=true (or the ORCHICON_MCP_INSTALL_DRYRUN=1 env gate) resolves what WOULD run without executing or writing.",
			Mutating:    true,
			Fn:          toolInstallMCPServer,
			Properties:  map[string]PropertySchema{"id": {Type: "string", Description: "MCP server ID"}, "dry_run": {Type: "boolean", Description: "Resolve the install plan without executing (default false)"}},
			Required:    []string{"id"},
		},
		{
			Name:        "list_mcp_catalog",
			Description: "List the built-in curated registry of popular MCP servers (filesystem, github, gitlab, postgres, sqlite, fetch, playwright, sentry, slack, and more) with install specs (npx/uvx/docker/remote_url), default config, docs links, and required secrets. One-click add = read an entry, then create_mcp_server with catalog_slug.",
			Mutating:    false,
			Fn:          toolListMCPCatalog,
		},
		{
			Name:        "set_mcp_server_secret",
			Description: "Store a credential for an MCP server entry via the tenant secrets store (AES-256-GCM at rest, same pattern as provider tokens). The key must be a required secret or an existing env/header key of the entry. Values are never returned.",
			Mutating:    true,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolSetMCPServerSecret(ctx, pool, secretsKEK, args)
			},
			Properties: map[string]PropertySchema{"id": {Type: "string", Description: "MCP server ID"}, "name": {Type: "string", Description: "Env/header key or required secret name (e.g. GITHUB_PERSONAL_ACCESS_TOKEN)"}, "value": {Type: "string", Description: "Secret value (plaintext, encrypted at rest)"}},
			Required:   []string{"id", "name", "value"},
		},
		{
			Name:        "clear_mcp_server_secret",
			Description: "Delete a stored credential for an MCP server entry from the tenant secrets store.",
			Mutating:    true,
			Fn: func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
				return toolClearMCPServerSecret(ctx, pool, secretsKEK, args)
			},
			Properties: map[string]PropertySchema{"id": {Type: "string", Description: "MCP server ID"}, "name": {Type: "string", Description: "Env/header key or required secret name"}},
			Required:   []string{"id", "name"},
		},
		{
			Name:        "set_project_mcp_servers",
			Description: "Replace a project's MCP server selection (references, never copies). Editing an entry updates every consumer automatically.",
			Mutating:    true,
			Fn:          toolSetProjectMCPServers,
			Properties:  map[string]PropertySchema{"project_id": {Type: "string", Description: "Project ID"}, "ids": {Type: "array", Description: "MCP server IDs to select (empty = project defaults fall through to tenant default)"}},
			Required:    []string{"project_id", "ids"},
		},
		{
			Name:        "set_tenant_default_mcp_servers",
			Description: "Replace the tenant default MCP server set (used when a project/worker has no selection). References, never copies.",
			Mutating:    true,
			Fn:          toolSetTenantDefaultMCPServers,
			Properties:  map[string]PropertySchema{"ids": {Type: "array", Description: "MCP server IDs to select as tenant default (empty = no default)"}},
			Required:    []string{"ids"},
		},

		// --- Audit ---
		{
			Name:        "list_audit_events",
			Description: "List audit events for the current tenant — the actor-based 'who did what' trail (action, actor, auth method, target, before/after, trace id). Optional filters: action, actor_id, target_type, target_id, start_time, end_time. Returns a bounded, compact list — pass next_page_token to page through the rest. Read-only.",
			Mutating:    false,
			Fn:          toolListAuditEvents,
			Properties: map[string]PropertySchema{
				"action":      {Type: "string", Description: "Optional exact action filter (e.g. work_item.created)"},
				"actor_id":    {Type: "string", Description: "Optional actor identity ID filter"},
				"target_type": {Type: "string", Description: "Optional target type filter (e.g. work_item)"},
				"target_id":   {Type: "string", Description: "Optional target ID filter"},
				"start_time":  {Type: "string", Description: "Optional RFC3339 inclusive lower bound on occurred_at (e.g. 2026-08-15T12:00:00Z)"},
				"end_time":    {Type: "string", Description: "Optional RFC3339 exclusive upper bound on occurred_at (e.g. 2026-08-15T13:00:00Z)"},
				"page_size":   {Type: "number", Description: "Optional page size (max 1000, default 100)"},
				"page_token":  {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"},
			},
		},
	}
}
