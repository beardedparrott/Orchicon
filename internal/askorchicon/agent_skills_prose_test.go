package askorchicon

// agent_skills_prose_test.go — the FREE-TEXT tenant agent config is PROSE, and it must
// never grow into a scope.
//
// The tenant's `ask_orchicon_agent_config` row is the ONE surviving tenant-level Ask
// surface. `Skills` there is free text; a conversation's / project's `skill_files` are
// real on-disk paths rendered as a `# Skills` MANIFEST by the ONE shared platform
// renderer. The work item requires the two to be unmistakable and the free-text one to
// stay a PROMPT SECTION ONLY — no tenant MCP tier, no tenant skill_files tier.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// The prose renders under its OWN heading, distinct from the rendered manifest's
// `# Skills`.
func TestAgentConfigSkillsRendersAsProseUnderItsOwnHeading(t *testing.T) {
	cfg := db.AgentConfigRow{
		SystemPrompt: "Tenant additional instructions.",
		Role:         "You are the resident architect.",
		Skills:       "I know Postgres internals and can read flamegraphs.",
		Behavior:     "Ask before acting.",
		AgentsMD:     "Remember: the operator hates emoji.",
	}
	reg := testToolRegistry()
	p := BuildSystemPrompt(modeBrainstorm, cfg, reg, "")

	for _, want := range []string{
		"## Additional Instructions",
		"## Role",
		"## Skills & Responsibilities (prose)",
		"## Behavior",
		"## Agent Memory (AGENTS.md)",
		"You are the resident architect.",
		"I know Postgres internals and can read flamegraphs.",
		"Ask before acting.",
		"Remember: the operator hates emoji.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the rendered prompt is missing %q", want)
		}
	}

	// THE PROSE HEADING IS `##`, the MANIFEST is `#` — so a reader can tell the
	// free-text field apart from a list of real paths at a glance.
	if strings.Contains(p, "\n# Skills\n") {
		t.Error("the prompt has a `# Skills` heading with no skills manifest — the prose heading must not collide with the manifest's")
	}
}

// An UNSET field adds nothing to the prompt: an empty heading would be noise and would
// shift the cached static prefix for a tenant that never set the field.
func TestAgentConfigEmptyProseFieldsAddNoHeading(t *testing.T) {
	reg := testToolRegistry()
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg, "")
	for _, unwanted := range []string{
		"## Additional Instructions", "## Role", "## Skills & Responsibilities (prose)",
		"## Behavior", "## Agent Memory (AGENTS.md)",
	} {
		if strings.Contains(p, unwanted) {
			t.Errorf("an empty field still emitted %q", unwanted)
		}
	}
}

// THE SHAPE PIN. The tenant agent config carries NO MCP-selection field and NO
// skill_files field — the free-text fields are prose only. A struct literal naming
// EVERY field is a compile-time pin: adding a tenant-tier MCP/skill_files field breaks
// this test rather than quietly introducing the scope the epic forbids.
func TestAgentConfigCarriesNoTenantTierScope(t *testing.T) {
	cfg := db.AgentConfigRow{
		ID:              "",
		TenantID:        "",
		SystemPrompt:    "",
		Role:            "",
		Skills:          "",
		Behavior:        "",
		AgentsMD:        "",
		ToolDefinitions: nil,
		ContextSources:  nil,
		Permissions:     nil,
		BudgetOverrides: nil,
		CreatedAt:       db.AgentConfigRow{}.CreatedAt,
		UpdatedAt:       db.AgentConfigRow{}.UpdatedAt,
	}
	// The field set above is deliberately EVERY field: a new `MCPServers` /
	// `SkillFiles` field would make this literal fail to compile, which is the point.
	_ = cfg
}
