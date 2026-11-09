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
	}
	readTools = map[string]bool{
		"read": true, "batch_read": true, "grep": true, "batch_grep": true,
		"list": true, "glob": true, "todoread": true,
	}
	bashTools = map[string]bool{
		"bash": true, "shell": true,
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
