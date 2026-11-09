// Package askmode holds the Ask Orchicon MODE policy in a place NO adapter owns.
package askmode

import (
	"context"
	"strings"
)

// The modes, as the DB stores them (askorchicon's conversation mode vocabulary).
const (
	Brainstorm = "brainstorm"
	Iteration  = "iteration"
	QuickWork  = "quick_work"
)

// --- the turn's mode -------------------------------------------------------------------------

// ctxKey is the unexported type of the context key, so no other package can collide with it.
type ctxKey struct{}

// WithMode stamps a context with the mode a turn is running under.
func WithMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, ctxKey{}, mode)
}

// ModeFromContext reads a turn's mode.
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
	// MayAct reports whether this mode may perform an ACTION.
	MayAct bool
}

// The READ-ONLY suite stays — read, batch_read, grep, batch_grep, glob, list, list_project_dir,
// read_project_file, ask_file_root — because understanding a project is what the other modes are
// for.
func theWorkTools() map[string]bool {
	return map[string]bool{
		"write": true, "edit": true, "batch_write": true, "bash": true,
	}
}

// The Planners: the tools that AUTHOR OR DISPATCH the work.
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
		// call a third-party MCP tool directly — it dispatches a worker that has that server.
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

// orchiconMCPServer is the platform's OWN MCP sidecar's server name (`mcp__orchicon__<tool>`,
// mcpconfig.go).
const orchiconMCPServer = "orchicon"

// IsOpaqueMCPTool reports whether name is an MCP tool the platform CANNOT classify:
// `mcp__<server>__<tool>` for a server that is NOT the platform's own `orchicon` sidecar.
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

// MayAct reports whether a mode may perform an ACTION (see Policy.MayAct).
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

// DeniedNames returns a mode's denied tool names as a slice, for an adapter that wants to apply the
// policy through its OWN mechanism (a config block, a flag.
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
