package workerrestrict

import "strings"

// antigravity.go — the agy (Antigravity) spelling of the built-in subagent tools.
//
// WHY A SEPARATE LIST INSTEAD OF ADDING TO SubagentToolNames. That slice is pinned by the claude and
// opencode tests (and by claude's `--disallowedTools` render), so appending to it would silently change
// another adapter's launch argv. The SET is the same policy — a worker may never spawn a subagent,
// because Orchicon already splits the work into focused steps — and each adapter spells the tool
// differently: claude says Task/Agent, opencode says task, agy says invoke_subagent/define_subagent.
// One list per adapter keeps the policy single while the spelling stays truthful per transport.

// AntigravitySubagentToolNames lists every spelling of agy's subagent surface, so a deny cannot be
// evaded by the CLI naming it differently. `invoke_subagent` runs one delegated task;
// `define_subagent` registers a new one, which is the same capability one step removed.
var AntigravitySubagentToolNames = []string{"invoke_subagent", "define_subagent"}

// IsAntigravitySubagentTool reports whether a tool name is one of agy's built-in subagent tools.
// Case-insensitive and trimmed: agy's names are lower_snake today, but a future release could
// title-case them, and a deny that misses on spelling is a deny that does not exist.
func IsAntigravitySubagentTool(toolName string) bool {
	name := strings.ToLower(strings.TrimSpace(toolName))
	for _, n := range AntigravitySubagentToolNames {
		if name == n {
			return true
		}
	}
	return false
}
