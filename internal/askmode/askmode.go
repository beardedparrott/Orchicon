// Package askmode holds the Ask Orchicon MODE policy in a place NO adapter owns.
//
// WHY IT IS ITS OWN PACKAGE. The boundary started life inside internal/askorchicon, which was tolerable while the
// native adapter was the only one that could enforce it — but it makes the policy a property of one adapter, and
// the operator is adding more: "we should definitely build the agnostic approach now for all adapters instead of
// waiting. New adapter support is coming very soon." A table that lives in the native adapter's package has to be
// re-homed (and re-tested) the first time a second adapter needs it, so it lives here instead: it imports
// nothing, and every adapter reads the same rule.
//
// WHAT IS HERE vs. WHAT IS NOT. This package decides WHICH tools a mode may run — the table, and the turn's mode
// as a context value. It says nothing about HOW a mode is refused: the wording is the caller's (the native
// adapter's refusal is a tool error carrying a message written for the model to relay; a different adapter might
// deny a tool at its own layer and never need a message at all).
package askmode

import "context"

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
	},
	QuickWork: {
		Mode:   QuickWork,
		Denied: theWorkTools(),
		Why: "Quick Work DISPATCHES; it does not do the work itself. It creates an ephemeral worker, workflow " +
			"and work item, fires the run and reports back — and it never edits code in this conversation.",
		SwitchTo: Iteration,
	},
	Iteration: {
		Mode:   Iteration,
		Denied: thePlanTools(),
		Why: "Iteration is the hands: it cuts the branch, edits the code, runs the tests and commits. It does " +
			"not author the plan or dispatch the pipeline — a working session that turns into a planning " +
			"session is exactly what this mode exists to avoid.",
		SwitchTo: Brainstorm,
	},
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
func Allows(mode, tool string) bool {
	p, ok := policies[mode]
	if !ok {
		return true
	}
	return !p.Denied[tool]
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
