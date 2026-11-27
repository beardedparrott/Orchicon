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
		// THE ANTIGRAVITY (agy) SPELLINGS of the same two actions, and both recorded ones are listed: agy
		// 1.5.13 emits `write_file`/`edit_file`, while ACP's documentation and the recordings also carry
		// `write_to_file`/`replace_file_content`/`multi_replace_file_content`. A table that knew only one
		// spelling would classify the other half as `Other`, and an activity line would then report a
		// write-heavy session as if it had modified nothing.
		"write_file": true, "write_to_file": true, "create_file": true,
		"edit_file": true, "replace_file_content": true, "multi_replace_file_content": true,
		"apply_patch": true,
		// set_workdir MODIFIES session state rather than the filesystem: it rebinds the anchor every
		// subsequent relative write and shell command resolves against. It belongs in Modify (not Bash)
		// because nothing about it runs a shell, and it must not be Ignore — a ledger that omitted it
		// would report a session whose writes went somewhere with no record of why they moved.
		"set_workdir": true,
	}
	readTools = map[string]bool{
		"read": true, "batch_read": true, "grep": true, "batch_grep": true,
		"list": true, "glob": true, "todoread": true,
		// The agy spellings of the same reads (internal/antigravity/toolnames.go canonicalToolNames
		// maps them onto read/grep/ls/glob).
		"read_file": true, "view_file": true, "open_file": true,
		"grep_search": true, "list_dir": true, "list_directory": true,
		"find_by_name": true,
	}
	bashTools = map[string]bool{
		"bash": true, "shell": true,
		// agy's shell tool.
		"run_command": true,
	}
)

// orchiconPrefix is the MCP-style prefix the Ask prompt advertises and the real name on the
// opencode host (askmode.NormalizeToolName strips the same one).
const orchiconPrefix = "orchicon_"

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
	// `orchicon_` is a spelling of the same name, not a different tool.
	n = strings.TrimPrefix(n, orchiconPrefix)
	// AGY'S MCP SPELLING IS A PREFIX TOO, and it is a DIFFERENT shape: `mcp_orchicon_orchicon_ask_user`
	// (single underscores, and the tool keeps its own `orchicon_` prefix). Without this arm the platform's
	// own ask card classifies as `Other` — real work it is not — and every antigravity Ask turn would show
	// an activity line counting the questions it asked.
	if rest, ok := strings.CutPrefix(n, "mcp_orchicon_"); ok {
		n = strings.TrimPrefix(rest, orchiconPrefix)
	}
	// ask_user is a CARD, not work — every spelling the clients use.
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
