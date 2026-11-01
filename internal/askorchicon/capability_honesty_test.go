package askorchicon

// capability_honesty_test.go — THE RULE ABOUT AN APPARENTLY MISSING CAPABILITY, IN EVERY MODE.
//
// REPORTED: an Ask session concluded that its mcp__aseprite__* tools were unavailable — from a LIST of
// definitions, which answers a different question from "what am I offered" — and spent the whole session driving
// the MCP server over stdio JSON-RPC through bash. That flattens structured and binary results to text, so every
// image arrived as prose: blind drawing, two rejected drafts. The platform's own log recorded the servers
// registered the whole time.
//
// Two things had to change for that not to repeat: a tool that answers "what do I have" honestly (ask_mcp_tools,
// tested in ask_mcp_tools_test.go) and this rule, which is the part that tells a model to use it rather than build
// a workaround. It is written ONCE, in the session-contract writer every mode goes through, so a fourth mode
// cannot miss it.

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
//
// "Do I have this?" has three answers, and conflating them is the whole failure: it is absent, it is mounted and
// offered, or it is mounted and WITHHELD BY THE MODE. Without the third, a mode refusal reads as a missing
// capability and invites a workaround for something the platform is deliberately gating.
func TestTheRuleNamesTheWithheldCase(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg, "")
	if !strings.Contains(p, "hosted_by_a_refused_mode") {
		t.Error("the rule does not name hosted_by_a_refused_mode, so a mode refusal is indistinguishable from a " +
			"missing server — which is the state that produced the reported workaround")
	}
}
