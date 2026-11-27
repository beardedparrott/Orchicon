// agy_tool_spelling_test.go — THE PREAMBLE MUST NAME A TOOL THAT ACTUALLY EXISTS.
//
// MEASURED against agy 1.5.13 (2026-11-27, this host): the ACP session is offered `mcp_orchicon_ask_user`.
// agy's MCP spelling is `mcp_<server>_<tool>` with SINGLE underscores, and this server is registered as
// `orchicon` while the tool it serves is named `ask_user` — so the joined name is `mcp_orchicon_ask_user`,
// NOT `mcp_orchicon_orchicon_ask_user`. Two independent live probes agree:
//
//	TestAskLive_AskUserToolReachesTheModel   tools seen = [mcp_orchicon_ask_user]
//	a plain GUI conversation asked to list its tools: mcp_orchicon_ask_user, mcp_orchicon_list_projects, …
//
// The preamble used to tell the model "agy spells it `mcp_orchicon_orchicon_ask_user`", which names nothing
// on the wire. A model that follows that instruction calls a tool that does not exist, gets an error, and
// falls back to writing the question as PROSE — which is exactly the GUI symptom observed: the assistant
// said "please choose … using the card above" while the transcript recorded NO tool call at all.
package antigravity

import (
	"strings"
	"testing"
)

// TestAgyToolSpellingMatchesTheWire pins the preamble to the name agy actually offers.
func TestAgyToolSpellingMatchesTheWire(t *testing.T) {
	p := agyAskPromptPreamble()

	if !strings.Contains(p, "mcp_orchicon_ask_user") {
		t.Fatalf("the preamble must name the tool agy actually offers (`mcp_orchicon_ask_user`); got:\n%s", p)
	}
	// The doubled-prefix spelling does not exist on the wire. Mentioning it teaches the model a name that
	// fails, so the preamble must not carry it at all.
	if strings.Contains(p, "mcp_orchicon_orchicon_ask_user") {
		t.Fatalf("the preamble names `mcp_orchicon_orchicon_ask_user`, which agy does not offer (measured: "+
			"`mcp_orchicon_ask_user`) — a model told that name calls nothing and falls back to prose:\n%s", p)
	}
	// It must still forbid agy's own tool, whose answer cannot come back over ACP.
	if !strings.Contains(p, "ask_question") {
		t.Fatalf("the preamble must keep forbidding agy's native `ask_question`: %s", p)
	}
}

// TestAskToolNameClassificationAcceptsTheMeasuredSpelling proves the MCP classifier recognises the name the
// wire really carries, so an agy ask call is treated as the platform's own tool (no permission card about a
// card) rather than as a third-party MCP server's tool.
//
// It checks only MCP-QUALIFIED names on purpose: `isOrchiconMCPTool` answers "is this one of the platform's
// tools reached over an MCP server", so a bare `ask_user`/`orchicon_ask_user` is correctly NOT an MCP tool.
// The bare spellings are covered by isAskToolName in internal/askorchicon (tool_ledger_test.go).
func TestAskToolNameClassificationAcceptsTheMeasuredSpelling(t *testing.T) {
	for _, name := range []string{
		"mcp_orchicon_ask_user",          // what agy actually sends (measured)
		"mcp_orchicon_orchicon_ask_user", // defensive: a doubled/legacy spelling must not slip the card
		"mcp__orchicon__ask_user",        // claude
	} {
		if !isOrchiconMCPTool(name) {
			t.Errorf("isOrchiconMCPTool(%q) = false, want true — the platform's own ask tool must be "+
				"recognised in every MCP-qualified spelling the clients and adapters emit", name)
		}
	}
	// And a third-party MCP tool must NOT be mistaken for the platform's.
	if isOrchiconMCPTool("mcp_github_create_issue") {
		t.Error("isOrchiconMCPTool(mcp_github_create_issue) = true, want false — a foreign MCP tool is not ours")
	}
}
