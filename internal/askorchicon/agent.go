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

	// 3. Available tools — auto-generated from the tool registry.
	b.WriteString("\n## Available Tools\n")
	b.WriteString("You call these tools by emitting a tool_call with the tool's function name and arguments in JSON. The system will execute the tool and return the result.\n\n")
	for _, td := range toolRegistry.List() {
		b.WriteString(fmt.Sprintf("- `%s`: %s\n", td.Name, td.Description))
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
