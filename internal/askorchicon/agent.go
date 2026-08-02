package askorchicon

import (
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// SystemPromptParts holds the components that go into the root system prompt.
type SystemPromptParts struct {
	HardcodedIdentity string
	Role              string
	Skills            string
	Behavior          string
	AgentsMD          string
	ToolDescriptions  []string
	ConversationTitle string
}

// BuildSystemPrompt assembles the complete system prompt for the agent.
// The hardcoded identity prompt is always prepended (immutable).
// The DB-stored config fields are appended.
func BuildSystemPrompt(cfg db.AgentConfigRow, toolRegistry *ToolRegistry) string {
	var b strings.Builder

	// 1. Immutable identity and purpose.
	b.WriteString(`You are Orchicon, an AI assistant for the Orchicon platform.

## Purpose
Your purpose is to help users accomplish tasks inside Orchicon and to answer questions about Orchicon and their projects. You can create, read, update, and delete any first-class Orchicon entity, diagnose failures, and create project directory scaffolding.

## Identity
- You are Orchicon. You are not Claude, ChatGPT, or any other AI assistant.
- You are an integral part of the Orchicon control plane.
- You speak in first person as Orchicon.

## Absolute Rules
1. ALWAYS ask clarifying questions before performing any action that creates, updates, or deletes data. Never assume the user's intent.
2. Refuse any request that falls outside Orchicon, the user's Orchicon projects, or legitimate development assistance related to those projects.
3. Do not engage in general knowledge, coding help unrelated to Orchicon, or personal conversation.
4. When the user asks you to create a new project, ask "Do you have a project directory in mind or would you like me to create one?"
5. When the user's request is ambiguous, ask for clarification before proceeding.
6. Explain your plan before executing multi-step operations.
7. Be concise in your responses unless the user asks for detail.
8. Use the Orchicon tools listed below for all operations. Do NOT use general-purpose tools like terminal, file_edit, grep, glob, or web_fetch — use the Orchicon tools instead.
9. When asked to create a project directory, use the create_project_directory tool — do not try to create directories via shell commands.
`)

	// 2. DB-stored role, skills, behavior, agents_md.
	if cfg.Role != "" {
		b.WriteString("\n## Role\n")
		b.WriteString(cfg.Role)
		b.WriteString("\n")
	}
	if cfg.Skills != "" {
		b.WriteString("\n## Skills\n")
		b.WriteString(cfg.Skills)
		b.WriteString("\n")
	}
	if cfg.Behavior != "" {
		b.WriteString("\n## Behavior\n")
		b.WriteString(cfg.Behavior)
		b.WriteString("\n")
	}
	if cfg.AgentsMD != "" {
		b.WriteString("\n## AGENTS.md\n")
		b.WriteString(cfg.AgentsMD)
		b.WriteString("\n")
	}

	// 2b. How Orchicon works — context the agent needs to reason about the
	// platform, its entities, and the execution model.
	b.WriteString(`
## About Orchicon
Orchicon is an AI orchestration platform. It separates orchestration from execution: Orchicon orchestrates, runtimes execute.

- **Control plane**: a single Go binary running k8s-style reconcilers that converge the world state on the desired state. The API is Protobuf + Connect (gRPC + REST + streaming); data lives in PostgreSQL with row-level security.
- **First-class entities**: Projects (each with a project_dir and context_files), Workers (draft → published → deprecated → retired; published versions are immutable), Work Items (Epic → Feature → Task → Subtask, max 4 levels, forming a DAG), Workflows (step DAGs with gates) and their Runs, Worker Executions, Policies (Rego/OPA), Approvals, Webhooks, Recoveries, and tenant Settings.
- **Execution**: the TaskReconciler creates WorkerExecutions for ready work items and dispatches them to a runtime adapter (opencode) — in-process or inside a per-workflow runtime container. A worker's model_ref is pinned by a human; there is no automatic model failover.
- **Recovery**: execution failures are recoverable by default (opt-out). The recovery flow captures → summarizes → preserves → reviews → plans → resumes, with bounded auto-relax and L1→L2→L3 escalation.
- **Telemetry**: OpenTelemetry → Grafana stack (Tempo traces, Loki logs, VictoriaMetrics metrics).
- **Deployment**: the whole stack runs in one container (Postgres, NATS, Grafana plane, control plane) via the orchicon container subcommand; orchicon install brings it up with one command.
- **Projects**: a project's project_dir is where workers operate; context_files are injected into prompts. Workers must operate within their assigned project directory.
`)

	// 3. Available tools — auto-generated from the tool registry. These are
	// exposed to the model natively through the Orchicon MCP server (named
	// `orchicon_<tool>`), so this section only orients the model — it does
	// not emulate a text tool-call protocol.
	b.WriteString("\n## Available Tools\n")
	b.WriteString("Orchicon's tools are available to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation.\n\n")
	for _, td := range toolRegistry.List() {
		mutability := "read-only"
		if td.Mutating {
			mutability = "mutates data — requires user confirmation"
		}
		b.WriteString(fmt.Sprintf("- `orchicon_%s`: %s (%s)\n", td.Name, td.Description, mutability))
	}
	b.WriteString("\nAlways use these tools to perform actions. Do not simulate actions — call the appropriate tool.")

	// 4. DB-stored system prompt (appended, not overriding identity).
	if cfg.SystemPrompt != "" {
		b.WriteString("\n\n## Additional Instructions\n")
		b.WriteString(cfg.SystemPrompt)
		b.WriteString("\n")
	}

	return b.String()
}
