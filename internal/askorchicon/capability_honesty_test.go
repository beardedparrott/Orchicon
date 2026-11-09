package askorchicon

// capability_honesty_test.go — THE RULE ABOUT AN APPARENTLY MISSING CAPABILITY, IN EVERY MODE.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestEveryModeIsToldToCheckBeforeWorkingAroundAGap(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	for _, mode := range []string{modeIteration, modeBrainstorm, modeQuickWork} {
		p := BuildSystemPrompt(mode, db.AgentConfigRow{}, reg, "")
		for _, want := range []string{
			// The heading, so a reader can find it.
			"Capabilities: check, then act",
			// The ACT: one direct attempt is cheaper than a workaround.
			"make ONE direct attempt",
			// The DISTINCTION that caused the detour: a list is not the mounted surface.
			"NOT evidence of absence",
			// The tool that answers the real question, by name — a rule without the tool sends the model back to
			// guessing.
			"ask_mcp_tools",
			// The specific harm: a shell flattens structured results.
			"Never route a tool call through a shell",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s mode prompt is missing %q — a model in this mode could repeat the reported detour",
					mode, want)
			}
		}
	}
}

// TestTheRuleNamesTheWithheldCase — the third state a model must be able to tell apart.
func TestTheRuleNamesTheWithheldCase(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg, "")
	if !strings.Contains(p, "hosted_by_a_refused_mode") {
		t.Error("the rule does not name hosted_by_a_refused_mode, so a mode refusal is indistinguishable from a " +
			"missing server — which is the state that produced the reported workaround")
	}
}
