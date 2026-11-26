package askorchicon

import "testing"


// TestIsAskToolNameCoversEveryAdapterSpelling is the three-adapter pin: the native bridge records
// `orchicon_ask_user`, claude records the MCP-qualified `mcp__orchicon__ask_user`, and agy (Antigravity)
// spells MCP tools `mcp_<server>_<tool>` with SINGLE underscores while the tool keeps its own prefix — so its
// card arrives as `mcp_orchicon_orchicon_ask_user`.
//
// WITHOUT THE AGY ARM the ask is recorded as an ordinary tool call, which is exactly the "the model says it
// sent a card and no card appeared" report this package's ask_card_promote.go exists to kill.
func TestIsAskToolNameCoversEveryAdapterSpelling(t *testing.T) {
	for _, name := range []string{
		"ask_user",                       // the registry key
		"orchicon_ask_user",              // the native bridge's spelling
		"mcp__orchicon__ask_user",        // claude's MCP-qualified spelling
		"mcp_orchicon_orchicon_ask_user", // agy / Antigravity
		"mcp_orchicon_ask_user",          // agy without the tool's own prefix
		"mcp__some_other_server__ask_user",
	} {
		if !isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = false, want true: that ask would never render as a card", name)
		}
	}
	// And it must NOT swallow everything ending in "user": a different tool is a different tool.
	for _, name := range []string{"", "ask", "create_work_item", "orchicon_create_work_item", "get_user"} {
		if isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = true, want false", name)
		}
	}
}
