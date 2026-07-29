package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ToolIntent represents a detected tool call from the user's message.
type ToolIntent struct {
	ToolName string
	Args     json.RawMessage
}

// DetectToolIntents analyzes the user's message and detects tool-callable
// intents. Read-only tools are returned for automatic execution.
// Mutating tools are detected but NOT returned (model must ask first).
func (r *ToolRegistry) DetectToolIntents(msg string) []ToolIntent {
	lower := strings.ToLower(msg)
	var intents []ToolIntent

	// Mapping: keywords → tool name → args (nil for list tools).
	patterns := []struct {
		keywords []string
		tool     string
		args     json.RawMessage
	}{
		// --- Read-only list tools ---
		{[]string{"list projects", "show projects", "what projects", "my projects", "all projects"}, "list_projects", nil},
		{[]string{"list work items", "show work items", "what work items", "all work items", "my work items", "what do i need to do", "what's on my plate"}, "list_work_items", nil},
		{[]string{"list workers", "show workers", "what workers", "all workers"}, "list_workers", nil},
		{[]string{"list workflows", "show workflows", "what workflows", "all workflows"}, "list_workflows", nil},
		{[]string{"list executions", "show executions", "all executions"}, "list_executions", nil},
		{[]string{"show settings", "get settings", "what are my settings", "current settings", "default model"}, "get_settings", nil},
		{[]string{"usage", "cost", "spend", "how much", "token usage", "billing"}, "get_usage", nil},

		// --- Read-only diagnostic tools ---
		{[]string{"diagnose", "why did it fail", "what went wrong", "failure", "error in", "troubleshoot"}, "diagnose_failure", nil},

		// --- Scheduling / update patterns ---
		// "schedule", "set time", "set [item] to scheduled" — refresh work items
		// so the model has current data to reference by number or title.
		{[]string{"schedule", "set time", "set to scheduled", "scheduled for", "schedule for", "scheduled_start"}, "list_work_items", nil},
	}

	for _, p := range patterns {
		for _, kw := range p.keywords {
			if strings.Contains(lower, kw) {
				// Only return non-mutating tools for automatic execution.
				td, ok := r.Get(p.tool)
				if ok && !td.Mutating {
					intents = append(intents, ToolIntent{ToolName: p.tool, Args: p.args})
				}
				break
			}
		}
	}
	return intents
}

// ToolFn is the signature for every tool function.
// It receives the tenant-scoped context, the DB pool, and JSON arguments,
// and returns a JSON result or an error.
type ToolFn func(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error)

// ToolDefinition describes a tool available to the agent.
type ToolDefinition struct {
	Name        string
	Description string
	Mutating    bool // if true, requires clarification before execution
	Fn          ToolFn
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
		},
		{
			Name:        "create_project",
			Description: "Create a new project. Requires title. Optionally accepts goals, project_dir, context_files.",
			Mutating:    true,
			Fn:          toolCreateProject,
		},
		{
			Name:        "update_project",
			Description: "Update an existing project's fields (title, goals, project_dir, context_files, status).",
			Mutating:    true,
			Fn:          toolUpdateProject,
		},
		{
			Name:        "delete_project",
			Description: "Delete a project by ID. This is irreversible.",
			Mutating:    true,
			Fn:          toolDeleteProject,
		},
		{
			Name:        "create_project_directory",
			Description: "Create a project directory on the filesystem and set it as the project's project_dir. Optionally scaffold subdirectories (src, docs, tests, workflows).",
			Mutating:    true,
			Fn:          toolCreateProjectDir,
		},

		// --- Work Items ---
		{
			Name:        "list_work_items",
			Description: "List work items for a project or tenant. Supports filter by status, kind, search.",
			Mutating:    false,
			Fn:          toolListWorkItems,
		},
		{
			Name:        "get_work_item",
			Description: "Get a single work item by ID.",
			Mutating:    false,
			Fn:          toolGetWorkItem,
		},
		{
			Name:        "create_work_item",
			Description: "Create a new work item within a project. Requires title and project_id. Optionally accepts parent_id, kind, description, acceptance_criteria, priority.",
			Mutating:    true,
			Fn:          toolCreateWorkItem,
		},
		{
			Name:        "update_work_item",
			Description: "Update a work item's fields (title, description, status, priority, etc.).",
			Mutating:    true,
			Fn:          toolUpdateWorkItem,
		},
		{
			Name:        "schedule_work_item",
			Description: "Schedule a work item: set its status to scheduled and optionally set a start time. Provide work_item_id and optionally scheduled_time (ISO 8601 or 'N minutes from now').",
			Mutating:    true,
			Fn:          toolScheduleWorkItem,
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
		},
		{
			Name:        "create_worker",
			Description: "Create a new worker with a name and optional runtime/model configuration.",
			Mutating:    true,
			Fn:          toolCreateWorker,
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
		},
		{
			Name:        "create_workflow",
			Description: "Create a new workflow with a name and optional template.",
			Mutating:    true,
			Fn:          toolCreateWorkflow,
		},

		// --- Executions ---
		{
			Name:        "list_executions",
			Description: "List executions, optionally filtered by project, status, or workflow run.",
			Mutating:    false,
			Fn:          toolListExecutions,
		},
		{
			Name:        "get_execution",
			Description: "Get a single execution by ID, including conversation, output, and error details.",
			Mutating:    false,
			Fn:          toolGetExecution,
		},
		{
			Name:        "cancel_execution",
			Description: "Cancel a running execution by ID. Provide a reason.",
			Mutating:    true,
			Fn:          toolCancelExecution,
		},

		// --- Telemetry / Diagnostics ---
		{
			Name:        "diagnose_failure",
			Description: "Diagnose why a workflow or execution failed. Provide the execution ID or workflow run ID to get failure analysis.",
			Mutating:    false,
			Fn:          toolDiagnoseFailure,
		},
		{
			Name:        "get_usage",
			Description: "Get token usage and cost data. Optionally filter by project, time range.",
			Mutating:    false,
			Fn:          toolGetUsage,
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
		},
	}
}
