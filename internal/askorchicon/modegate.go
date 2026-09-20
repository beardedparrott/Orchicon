package askorchicon

// modegate.go — THE MODE BOUNDARY, ENFORCED BY THE PLATFORM RATHER THAN ASKED FOR IN PROSE.
//
// The operator: "Each mode of Ask Orchicon must NEVER just do the work it is not supposed to do and must always
// enforce the user switch the mode first no matter what the user says."
//
// "No matter what the user says" is the part that cannot be satisfied by an instruction. A user can argue any
// model past its prompt — that is what a prompt is — so the boundary is a REFUSAL at the layer that executes the
// call. The prompt tells the model what it is and how to explain the boundary; this decides it.
//
// IT IS ALSO THE TIDIER HALF OF THE FIX. Before this, the boundary lived only in prose, and the prose was
// contradicted by the rest of the prompt: Brainstorm was told it could take direct action ("you may take direct
// action when the user explicitly asks for it"), and its own next-step fork offered "Work directly with me". A
// prompt cannot be argued out of a permission it was explicitly granted. That language is gone (see agent.go)
// and this is what makes its removal hold.

import (
	"context"
	"fmt"
)

// askModeCtxKey carries the conversation's mode for the duration of one TURN.
//
// A context value rather than a field on the tool provider, and that is a correctness choice rather than style:
// the provider is built once per service (server.go: SetAskTools(deps.AskService.NativeAskTools())) and shared by
// every conversation, so per-turn mode state on it would be a data race between two conversations in different
// modes. The turn's context already flows from the dispatch site to ExecuteAskTool and to AskToolDefs, so the
// value rides the same path.
//
// It is set by the TURN, from conv.Mode, at the one place the conversation is loaded (see chat.go) — so a
// mid-conversation switch is picked up on the very next dispatch with no session change, which is what the mode
// toggle already promised.
type askModeCtxKey struct{}

// withAskMode stamps a turn's context with the mode it is running under.
func withAskMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, askModeCtxKey{}, mode)
}

// askModeFromContext reads the mode stamped on a turn. "" when there is none — a test invoking a tool directly,
// or a path that bypasses dispatch — and "" is treated as ALLOW (see modeAllowsTool): an unstamped context is
// not a mode, and refusing every tool because the stamp is missing would turn a plumbing slip into a dead
// session.
func askModeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	m, _ := ctx.Value(askModeCtxKey{}).(string)
	return m
}

// modeToolDeny is one mode's refusal set.
type modeToolDeny struct {
	// tools are the tool names this mode REFUSES, exactly as the registry is keyed (bare, no `orchicon_`
	// prefix) — normalizeAskToolName runs before the check, so the model may spell them either way.
	tools map[string]bool
	// why is the mode's own sentence, in the operator's terms.
	why string
	// switchTo names the mode that DOES this, so the refusal is a direction rather than a wall.
	switchTo string
}

// theWorkTools are the tools that DO THE WORK: they change code or run commands. Refused by every mode that is
// not Iteration — which is the operator's boundary in one line: "when it comes to actual work, that is when it
// is locked INSIDE orchicon tools. When it comes to the ACTION phase."
//
// bash IS IN HERE, and the operator ruled on it explicitly. It can only ever READ (git log, rg, cat), but it can
// also write, so allowing it would make the whole boundary porous. Brainstorm keeps the read-only suite — read,
// batch_read, grep, batch_grep, glob, list, list_project_dir, read_project_file, ask_file_root — which is what
// "understand a user's project inside and out and ... make suggestions" actually requires.
func theWorkTools() map[string]bool {
	return map[string]bool{
		"write": true, "edit": true, "batch_write": true, "bash": true,
	}
}

// thePlanTools are the tools that AUTHOR OR DISPATCH the plan. Refused by Iteration, because writing the plan and
// firing it are the other two modes' jobs.
//
// The operator, on the one they cared about most: "Yes it is a hard block on create work item. That is
// brainstorm's job or ephemeral work items for quick_work mode." The dispatch set is here for the same reason
// Iteration's own prose already gave ("do NOT propose firing workflows or schedules") — a prohibition in prose is
// what this file exists to replace.
//
// LIFECYCLE IS DELIBERATELY NOT HERE. archive/restore/delete_work_item are hygiene rather than authoring or
// dispatch, and blocking them could strand a mistake an iteration session was fixing. Flagged as a judgement
// call rather than smuggled in.
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

// modeDeniedTools is the whole boundary, as one table.
//
// ONE TABLE because the modes' relationship IS the table: Brainstorm and Quick Work refuse THE SAME set (the
// work) and differ in what they do INSTEAD, while Iteration refuses the other set (the plan). Writing that out
// three times is how the three would drift.
var modeDeniedTools = map[string]modeToolDeny{
	modeBrainstorm: {
		tools: theWorkTools(),
		why: "Brainstorm plans; it does not do the work. It designs, researches, asks clarifying questions and " +
			"writes the plan — a work item, a design, a decision — and the doing is a different mode's job.",
		switchTo: modeIteration,
	},
	modeQuickWork: {
		tools: theWorkTools(),
		why: "Quick Work DISPATCHES; it does not do the work itself. It creates an ephemeral worker, workflow " +
			"and work item, fires the run and reports back — and it never edits code in this conversation.",
		switchTo: modeIteration,
	},
	modeIteration: {
		tools: thePlanTools(),
		why: "Iteration is the hands: it cuts the branch, edits the code, runs the tests and commits. It does " +
			"not author the plan or dispatch the pipeline — a working session that turns into a planning " +
			"session is exactly what this mode exists to avoid.",
		switchTo: modeBrainstorm,
	},
}

// modeAllowsTool reports whether the mode may run tool, and when it may not, the refusal to hand back.
//
// AN EMPTY OR UNKNOWN MODE ALLOWS EVERYTHING, deliberately. The mode arrives from a DB column, so a value written
// by an older build or hand-edited must not silently disable a whole session's tools; and a caller that passes no
// mode is not making a claim about the boundary. The default is the behaviour that existed before this file —
// the safe direction for a default to be wrong in.
func modeAllowsTool(mode, tool string) (bool, string) {
	if mode == "" {
		return true, ""
	}
	d, ok := modeDeniedTools[mode]
	if !ok {
		return true, ""
	}
	if !d.tools[tool] {
		return true, ""
	}
	// THE REFUSAL IS WRITTEN FOR THE MODEL TO RELAY. It states the boundary, says the PLATFORM refuses it rather
	// than the model choosing to, and names the one action that resolves it — the user switching the mode. That
	// last part is the operator's requirement made literal ("must always enforce the user switch the mode
	// first"), and it is true: there is no mode-setting tool for the model to call instead.
	return false, fmt.Sprintf(
		"REFUSED BY THE PLATFORM: %q is not available in %s mode, so it was NOT executed. %s\n\n"+
			"You cannot override this, and you cannot switch your own mode — only the user can, from the mode "+
			"selector on this conversation. Say plainly that this is a %s job, name the mode, and ASK THE USER "+
			"TO SWITCH TO %s. Do not attempt the call again, and do not work around it with another tool.",
		tool, modeLabel(mode), d.why, modeLabel(d.switchTo), modeLabel(d.switchTo))
}
