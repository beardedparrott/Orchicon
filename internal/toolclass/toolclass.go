// Package toolclass is the ONE tool-name -> class map both clients render an activity line from.
//
// WHY A NEW PACKAGE (and not askmode, orchicon or tui/chat). The classifier's vocabulary is the
// union of vocabularies that already live in internal/askorchicon and internal/askmode, but the
// package that renders it is the TUI (internal/tui/chat), which must NOT import either of those:
// askorchicon pulls in orchicon, db, scheduler and providers, and growing the TUI's chat package
// that dependency to print one line is a worse trade than a tiny leaf package. So this is the
// LOWEST COMMON package reachable from both the Go caller and the shared cross-client fixture
// test. It imports nothing but the standard library, deliberately.
//
// WHY THE CLASS MAPS ARE NOT ASKED FROM askmode/orchicon. Reconciliation happens by TEST
// (reconcile_test.go, package toolclass_test), not by import: the test asserts these sets agree
// with askmode.PolicyFor(Brainstorm).Denied (— the exported proxy for askmode's unexported
// theWorkTools(), internal/askmode/askmode.go:90-94) and with orchicon.ConsentMutatingTools
// (internal/orchicon/chatturn.go:1600-1602) on the four mutating names. A test can import both;
// production code cannot without dragging them into every client.
//
// WHY THIS IS COARSER THAN askorchicon.toolIntentVerb (internal/askorchicon/consent.go:1033-1041),
// AND DELIBERATELY NOT MERGED WITH IT. toolIntentVerb produces a human VERB for one call
// ("modify", "delete", "run a shell command", "run a tool") by substring matching; this package
// produces one of THREE display BUCKETS for a counter. Merging them would either lose the delete
// verb or gain a bucket nobody asked for. They stay separate; the reconcile test makes their
// disagreement impossible to overlook.
package toolclass

import "strings"

// Class is a tool call's contribution to the activity line.
type Class int

const (
	// Modify changes the work: it writes code or files.
	Modify Class = iota
	// Read inspects the work: it reads, greps, lists or globs.
	Read
	// Bash runs a shell command. It is counted SEPARATELY from Modify even though the consent
	// policy puts `bash` in the same "doers" set as the write tools (askmode.go:90-94): the line
	// distinguishes "edits" from "commands" because the operator reads those as different work.
	// The reconcile test asserts `bash` is a MUTATING class, not that it is Modify.
	Bash
	// Ignore contributes nothing. It covers ask_user (a card, not work), the synthetic
	// permission.* records, MCP tools, and every name this package does not recognise.
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
// consent decision (internal/askorchicon/tool_ledger.go:74: FunctionName is "permission."+verdict,
// e.g. "permission.allow_once"). Counting one would let an APPROVAL inflate the tool tally.
const permissionPrefix = "permission."

// Classify maps a tool name to its class. Case-insensitive and whitespace-tolerant, because the
// name arrives from a model (and from a JSON column).
//
// AN UNRECOGNISED NAME IS ALWAYS Ignore. That is the load-bearing default (AC4): a tool this
// package has never heard of — an operator's MCP server (`mcp__github__create_issue`), a future
// product tool, a misspelling — is never counted into a class it does not belong to.
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
	switch {
	case modifyTools[n]:
		return Modify
	case readTools[n]:
		return Read
	case bashTools[n]:
		return Bash
	}
	// Everything else: MCP (`mcp__*`, including `mcp__orchicon__*`), product tools outside this
	// line's three buckets, `todowrite` (see summarize.go's D7 note), and unknown names.
	return Ignore
}
