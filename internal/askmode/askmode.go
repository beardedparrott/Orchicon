// Package askmode holds the Ask Orchicon MODE policy in a place NO adapter owns.
//
// WHY IT IS ITS OWN PACKAGE. The boundary started life inside internal/askorchicon, which was tolerable while the
// native adapter was the only one that could enforce it — but it makes the policy a property of one adapter, and
// the operator is adding more: "we should definitely build the agnostic approach now for all adapters instead of
// waiting. New adapter support is coming very soon." A table that lives in the native adapter's package has to be
// re-homed (and re-tested) the first time a second adapter needs it, so it lives here instead: every adapter
// reads the same rule, and it imports nothing but `strings` (for name normalisation).
//
// WHAT IS HERE vs. WHAT IS NOT. This package decides WHICH tools a mode may run — the table, and the turn's mode
// as a context value. It says nothing about HOW a mode is refused: the wording is the caller's (the native
// adapter's refusal is a tool error carrying a message written for the model to relay; a different adapter might
// deny a tool at its own layer and never need a message at all).
package askmode

import (
	"context"
	"strings"
)

// The modes, as the DB stores them (askorchicon's conversation mode vocabulary).
//
// They are defined HERE so the policy table and the mode constants cannot drift: an adapter that spells a mode
// differently from this table is a mode the boundary silently does not apply to.
const (
	Brainstorm = "brainstorm"
	Iteration  = "iteration"
	QuickWork  = "quick_work"
)

// --- the turn's mode -------------------------------------------------------------------------

// ctxKey is the unexported type of the context key, so no other package can collide with it.
type ctxKey struct{}

// WithMode stamps a context with the mode a turn is running under.
//
// A context value rather than state on a shared tool provider, because the provider is built once and shared by
// every conversation — per-turn mode on it would be a data race between two conversations in different modes.
// The turn's context already flows from dispatch to every tool call, so the value rides that path for free.
func WithMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, ctxKey{}, mode)
}

// ModeFromContext reads a turn's mode. "" when there is none — a caller that never stamped one, or a test driving
// a tool directly — and "" is treated as ALLOW by Allows: an unstamped context makes no claim about the boundary,
// and refusing everything because a stamp went missing would turn a plumbing slip into a dead session.
func ModeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	m, _ := ctx.Value(ctxKey{}).(string)
	return m
}

// --- the policy ------------------------------------------------------------------------------

// Policy is one mode's refusal set: the tools it may not run, why, and where the work belongs instead.
type Policy struct {
	// Mode is the mode this policy is for.
	Mode string
	// Denied are the tool names the mode REFUSES, exactly as the Ask tool registry is keyed (bare, no
	// `orchicon_` prefix — name normalisation happens before the check, so a caller may spell them either way).
	Denied map[string]bool
	// Why is the mode's own sentence, in the operator's terms, for a caller that wants to explain the refusal.
	Why string
	// SwitchTo names the mode that DOES this, so a refusal can be a direction rather than a wall.
	SwitchTo string
	// MayAct reports whether this mode may perform an ACTION. It is the policy's own answer to "is this mode
	// the hands?", DERIVED FROM THIS TABLE rather than declared beside it: a mode may act exactly when the
	// table does not deny the work tools (theWorkTools).
	//
	// WHY IT EXISTS, AND WHY IT IS A FIELD RATHER THAN A SECOND LIST. MCP tool names are OPAQUE to the
	// platform: `mcp__github__create_issue` reveals nothing to a deny list about whether it acts. The only
	// honest reading of a name the table cannot classify is "this is an ACTION" — so an opaque MCP tool is
	// offered only to a mode that MAY ACT, and refused elsewhere. That predicate has to come from the ONE
	// table (there must be no second list of hidden tools); this field is that table's own answer, tied to
	// theWorkTools by a drift-guard test so it can never drift from the denial sets.
	MayAct bool
}

// The Doers: the tools that CHANGE the work — they write code or run commands. Refused by every mode that is not
// the hands, which is the operator's boundary in one line: "when it comes to actual work, that is when it is
// locked INSIDE orchicon tools. When it comes to the ACTION phase."
//
// `bash` is denied, and the operator ruled on it explicitly. It can only ever READ (git log, rg, cat), but it can
// also write, so allowing it would make the boundary porous. The READ-ONLY suite stays — read, batch_read, grep,
// batch_grep, glob, list, list_project_dir, read_project_file, ask_file_root — because understanding a project
// is what the other modes are for.
func theWorkTools() map[string]bool {
	return map[string]bool{
		"write": true, "edit": true, "batch_write": true, "bash": true,
	}
}

// The Planners: the tools that AUTHOR OR DISPATCH the work. Refused by the hands, because writing the plan and
// firing the pipeline are the other two modes' jobs.
//
// LIFECYCLE IS DELIBERATELY ABSENT. archive/restore/delete_work_item are hygiene rather than authoring or
// dispatch, and blocking them could strand a mistake an iteration session was fixing.
func thePlanTools() map[string]bool {
	return map[string]bool{
		// authoring
		"create_work_item": true, "update_work_item": true,
		// sequencing and dispatch
		"schedule_work_item": true, "reorder_work_items": true, "control_sequence": true,
		// firing or advancing a run
		"force_progress_workflow_run": true, "retry_failed_workflow_run": true,
	}
}

// policies is the whole boundary, as one table.
//
// ONE TABLE because the modes' relationship IS the table: Brainstorm and Quick Work refuse THE SAME set (the
// work) and differ in what they do INSTEAD, while Iteration refuses the other set (the plan). Writing that out
// three times is how the three would drift.
var policies = map[string]Policy{
	Brainstorm: {
		Mode:   Brainstorm,
		Denied: theWorkTools(),
		Why: "Brainstorm plans; it does not do the work. It designs, researches, asks clarifying questions and " +
			"writes the plan — a work item, a design, a decision — and the doing is a different mode's job.",
		SwitchTo: Iteration,
		// MayAct is FALSE: the table denies the work tools, so this mode is not the hands. An opaque MCP
		// tool is an ACTION, so it is neither offered nor executed here (see Allows).
		MayAct: false,
	},
	QuickWork: {
		Mode:   QuickWork,
		Denied: theWorkTools(),
		Why: "Quick Work DISPATCHES; it does not do the work itself. It creates an ephemeral worker, workflow " +
			"and work item, fires the run and reports back — and it never edits code in this conversation.",
		SwitchTo: Iteration,
		// MayAct is FALSE: Quick Work dispatches rather than acts, so under the opaque-MCP rule it may not
		// call a third-party MCP tool directly — it dispatches a worker that has that server. The
		// platform cannot prove an opaque tool is a read, so it fails closed exactly as the work tools do.
		MayAct: false,
	},
	Iteration: {
		Mode:   Iteration,
		Denied: thePlanTools(),
		Why: "Iteration is the hands: it cuts the branch, edits the code, runs the tests and commits. It does " +
			"not author the plan or dispatch the pipeline — a working session that turns into a planning " +
			"session is exactly what this mode exists to avoid.",
		SwitchTo: Brainstorm,
		// MayAct is TRUE: the table does not deny the work tools, so this is the ONE mode that may act, and
		// therefore the ONE mode offered (and allowed to execute) an opaque MCP action tool.
		MayAct: true,
	},
}

// orchiconMCPServer is the platform's OWN MCP sidecar's server name (`mcp__orchicon__<tool>`, mcpconfig.go).
// It is the ONE MCP server whose tools are CLASSIFIED: its bare `<tool>` name (create_work_item, …) is already
// in the table, which is why the adapter strips the prefix before consulting the policy. Every OTHER MCP server
// is opaque to the platform.
const orchiconMCPServer = "orchicon"

// IsOpaqueMCPTool reports whether name is an MCP tool the platform CANNOT classify: `mcp__<server>__<tool>` for
// a server that is NOT the platform's own `orchicon` sidecar. Case-insensitive.
//
// THE DISTINCTION IS THE WHOLE MODE RULE. `mcp__orchicon__create_work_item` is classified by the table's bare
// name (`create_work_item`, a planner tool) — the adapter strips the prefix and the policy holds. An operator's
// server (`mcp__github__create_issue`) maps to no entry, so its bare name would be ALLOWED in every mode; the
// platform cannot tell `list_issues` from `create_issue` and must therefore treat the name as an ACTION.
//
// A name that is not an `mcp__` name at all (a native/host tool, a product tool spelled bare or with the
// `orchicon_` prefix) is NOT opaque — those ARE classified by the table's vocabulary.
func IsOpaqueMCPTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasPrefix(n, "mcp__") {
		return false
	}
	rest := n[len("mcp__"):]
	i := strings.Index(rest, "__")
	if i <= 0 {
		// `mcp__<something>` with no `__<tool>`: not the namespaced shape this rule reads.
		return false
	}
	server := rest[:i]
	tool := rest[i+2:]
	if server == "" || tool == "" {
		return false
	}
	return server != orchiconMCPServer
}

// NormalizeToolName maps a model-emitted tool name onto the table's bare vocabulary. Two prefixed
// spellings collapse onto the ONE name the policy table is keyed by:
//
//   - `orchicon_<tool>` — the MCP-style spelling the Ask prompt advertises, and the REAL name on the
//     opencode host, where the prefix IS the name;
//   - `mcp__orchicon__<tool>` — the spelling claude gives the platform's own sidecar's tools.
//
// The second exists so the SAME table classifies `mcp__orchicon__create_work_item` (a PLANNER tool)
// as it classifies `create_work_item` — the classification the claude hook already performs in
// claudeToolToPolicyName, now available to every adapter.
//
// It is exported so the native adapter's normalizeAskToolName can DELEGATE rather than keep a second
// copy of the rule: the table's vocabulary must have exactly one definition, or a tool the adapter
// normalises one way and the policy another is a boundary that silently does not hold.
func NormalizeToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if p := "mcp__" + orchiconMCPServer + "__"; strings.HasPrefix(strings.ToLower(trimmed), p) {
		return trimmed[len(p):]
	}
	return strings.TrimPrefix(trimmed, "orchicon_")
}

// MayAct reports whether a mode may perform an ACTION (see Policy.MayAct). An unknown or empty mode reports
// true, mirroring Allows: a caller that passes no mode is not making a claim about the boundary, and a value
// written by an older build must not silently disable a whole session.
func MayAct(mode string) bool {
	p, ok := policies[mode]
	if !ok {
		return true
	}
	return p.MayAct
}

// PolicyFor returns a mode's policy. ok=false for an unknown or empty mode — see Allows for why that is ALLOW.
func PolicyFor(mode string) (Policy, bool) {
	p, ok := policies[mode]
	return p, ok
}

// Allows reports whether mode may run tool.
//
// AN EMPTY OR UNKNOWN MODE ALLOWS EVERYTHING, deliberately. The mode arrives from a DB column, so a value written
// by an older build or hand-edited must not silently disable a whole session's tools; and a caller that passes no
// mode is not making a claim about the boundary. The default is the behaviour that existed before this file —
// the safe direction for a default to be wrong in.
//
// IT IS THE ONE DECISION FOR BOTH NAME CLASSES, and that is the design:
//
//   - an OPAQUE MCP tool (`mcp__<server>__<tool>`, server != `orchicon`) is an ACTION — allowed only in a mode
//     that MAY ACT. Its name maps to no table entry, so without this branch it is allowed in every mode
//     including Brainstorm, which is the hole this closes. The offered surface (the defs filter) and the
//     enforced surface (the call guard) both read THIS function, so there is no second list of hidden tools.
//   - anything else is the table's bare-name lookup, exactly as before, over the `orchicon_`-stripped name.
func Allows(mode, tool string) bool {
	p, ok := policies[mode]
	if !ok {
		return true
	}
	if IsOpaqueMCPTool(tool) {
		return p.MayAct
	}
	return !p.Denied[NormalizeToolName(tool)]
}

// DeniedNames returns a mode's denied tool names as a slice, for an adapter that wants to apply the policy through
// its OWN mechanism (a config block, a flag, a per-invocation allow-list) rather than asking this package about
// each tool. Sorted, so a policy handed to an adapter is deterministic and testable.
func DeniedNames(mode string) []string {
	p, ok := policies[mode]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(p.Denied))
	for name := range p.Denied {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// sortStrings is a plain insertion sort — these sets are four and seven entries, and importing sort for them
// would be noise in a package that otherwise imports nothing.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
