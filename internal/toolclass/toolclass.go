// Package toolclass is the ONE tool-name -> class map both clients render an activity line from.
package toolclass

import "strings"

// Class is a tool call's contribution to the activity line.
type Class int

const (
	// Modify changes the work: it writes code or files.
	Modify Class = iota
	// Read inspects the work: it reads, greps, lists or globs.
	Read
	// Bash runs a shell command.
	Bash
	// Other is real work this package cannot place in one of the three named buckets.
	Other
	// Ignore contributes nothing.
	Ignore
)

// classifySets are the ONE vocabulary. reconcile_test.go pins their agreement with the two
// existing exported definitions of the mutating set.
var (
	modifyTools = map[string]bool{
		"write": true, "edit": true, "batch_write": true,
		// set_workdir MODIFIES session state rather than the filesystem: it rebinds the anchor every
		// subsequent relative write and shell command resolves against. It belongs in Modify (not Bash)
		// because nothing about it runs a shell, and it must not be Ignore — a ledger that omitted it
		// would report a session whose writes went somewhere with no record of why they moved.
		"set_workdir": true,
		// ANTIGRAVITY'S SPELLINGS. agy ships both a recorded set (write_file/edit_file/read_file) and the
		// ACP-documented set (write_to_file/replace_file_content/view_file/…), and the antigravity adapter
		// emits the RAW agy tool name on an Ask permission event (the AC that pins `Tool:"run_command"`).
		// A name this package did not know would classify Other, and Other is REAL WORK counted as neither
		// a write nor a read — the activity line would report an agy session that edited files as if it had
		// done something unclassifiable.
		"write_to_file": true, "replace_file_content": true, "multi_replace_file_content": true,
		"write_file": true, "edit_file": true, "create_file": true, "apply_patch": true,
	}
	readTools = map[string]bool{
		"read": true, "batch_read": true, "grep": true, "batch_grep": true,
		"list": true, "glob": true, "todoread": true,
		// ANTIGRAVITY'S SPELLINGS, same reason as modifyTools.
		"view_file": true, "read_file": true, "open_file": true, "grep_search": true,
		"list_dir": true, "list_directory": true, "find_by_name": true,
		"read_url_content": true, "search_web": true, "web_search": true, "web_fetch": true,
	}
	bashTools = map[string]bool{
		"bash": true, "shell": true,
		// ANTIGRAVITY'S SPELLING.
		"run_command": true,
	}
)

// orchiconPrefix is the MCP-style prefix the Ask prompt advertises and the real name on the
// opencode host (askmode.NormalizeToolName strips the same one).
const orchiconPrefix = "orchicon_"

// agyOrchiconPrefix is how ANTIGRAVITY spells the platform's own MCP server: `mcp_orchicon_<tool>`
// (mcp_tool_call.jsonl records `mcp_echo_echo` for an operator server). claude spells the same server
// `mcp__orchicon__<tool>`, which already reaches Other via the generic MCP branch — but an agy ask card for
// `mcp_orchicon_orchicon_ask_user` would otherwise classify Other and put an ask card on the activity line
// as if it were work.
const (
	agyOrchiconPrefix    = "mcp_orchicon_"
	claudeOrchiconPrefix = "mcp__orchicon__"
)

// permissionPrefix marks the synthetic records toolLedger.recordPermission appends for every
// consent decision (internal/askorchicon/tool_ledger.go:74: FunctionName is "permission."+verdict.
const permissionPrefix = "permission."

// Classify maps a tool name to its class.
func Classify(toolName string) Class {
	n := strings.ToLower(strings.TrimSpace(toolName))
	// A consent decision is a record OF a decision, not work (tool_ledger.go:74).
	if strings.HasPrefix(n, permissionPrefix) {
		return Ignore
	}
	// `orchicon_` is a spelling of the same name, not a different tool — and so is the MCP-qualified form
	// BOTH adapters use for the platform's own sidecar (`mcp_orchicon_<tool>` on agy,
	// `mcp__orchicon__<tool>` on claude). Stripping the prefix is what makes `mcp_orchicon_orchicon_ask_user`
	// classify as the ask card it is rather than as unclassifiable work.
	n = strings.TrimPrefix(n, orchiconPrefix)
	n = strings.TrimPrefix(n, agyOrchiconPrefix)
	n = strings.TrimPrefix(n, claudeOrchiconPrefix)
	n = strings.TrimPrefix(n, orchiconPrefix)
	// ask_user is a CARD, not work — all three spellings the clients use.
	if n == "ask_user" || n == "askuser" || n == "ask_user_question" {
		return Ignore
	}
	// `todowrite` is session bookkeeping the model does every turn, not operator-meaningful work:
	// counting it would put a permanent counter on every line.
	if n == "todowrite" {
		return Ignore
	}
	switch {
	case modifyTools[n]:
		return Modify
	case readTools[n]:
		return Read
	case bashTools[n]:
		return Bash
	}
	// An empty name is not a call anybody made: it must not manufacture a count.
	if n == "" {
		return Ignore
	}
	// Everything else is REAL WORK, counted as Other: Orchicon product tools (`list_projects`,
	// `get_work_item`, …), MCP tools (`mcp__*`, including `mcp__orchicon__*`) and any name added later.
	return Other
}
