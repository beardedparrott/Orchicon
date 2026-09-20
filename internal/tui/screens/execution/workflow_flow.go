// workflow_flow.go — the workflow DAG rendered as a VERTICAL FLOW.
//
// Design decisions, and why they are not the GUI's:
//
//  1. NO CANVAS, and not as a compromise. `Step.position_x/position_y` are
//     PRESENTATION-ONLY: no reconciler or service reads them (grep finds only the
//     seeder). They exist so the GUI's react-flow canvas remembers where a box was
//     dragged. Measured across the live tenant's 229 published steps: 95 sit at
//     exactly (0,0), 100 carry no position at all, and the ones that do drift
//     diagonally (SDLC (Human) spans x −74→732, y −328→989) because they record a
//     DRAG, not a layout. So we COMPUTE the flow from `depends_on` — the only
//     field the machine actually reads — and never try to reproduce the canvas.
//
//  2. VERTICAL, on the operator's instruction. A long chain is the common case
//     (Force Workflow is 48 steps with ZERO joins), and a chain reads as a list,
//     which is what a vertical flow degenerates to naturally.
//
//  3. BRANCHES ARE NAMED, NOT DRAWN. The real graph-ness lives in the decision
//     steps' config (`success_branch` / `loop_branch` name other steps), not in
//     `depends_on`, which is linear. A back-edge drawn in ASCII either crosses
//     nodes or needs a routing channel; naming the target costs one line and is
//     unambiguous — the operator's own sketch: `[Retry? Yes] --> [Build 1]`.
//
//  4. Max fan-in across the live tenant is 2, so a fan-in is one extra line, not
//     a merge diagram.
package execution

import (
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// flowStep is one Step from a WorkflowVersion.steps JSON array (the proto carries
// the array as a JSON STRING). Positions are deliberately not parsed.
type flowStep struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	WorkerVer int32  `json:"worker_version"`
	GateRef   string `json:"gate_policy_ref"`
	Config    string `json:"config"`
	// DependsOn is decoded separately: the column is NOT type-consistent across
	// rows (207 arrays, 12 nulls, 10 of something else in the live tenant), so a
	// plain []string would fail the WHOLE unmarshal on one bad row.
	DependsOn json.RawMessage `json:"depends_on"`

	deps []string // parsed
}

// flowBranches are the named jumps a decision carries in its config JSON.
//
// They are read from the step's `config` because that is where they LIVE: the
// real graph-ness of a workflow is not in depends_on (which is linear) but in a
// decision naming the step it jumps to — including BACKWARDS, for a retry loop.
type flowBranches struct {
	Success  string `json:"success_branch"`
	Loop     string `json:"loop_branch"`
	MaxIter  int    `json:"max_iterations"`
	Reviewer string `json:"reviewer"`
	Conflict string `json:"conflict_value"`
}

// parseDepends tolerates every shape the column has been seen in:
// an array of ids, null, or a bare/empty/CSV string.
func parseDepends(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]string, 0, len(list))
		for _, s := range list {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// parseFlowSteps decodes a version's steps JSON. A malformed array yields no
// steps rather than an error: the caller renders "no steps" honestly.
func parseFlowSteps(stepsJSON string) []flowStep {
	s := strings.TrimSpace(stepsJSON)
	if s == "" {
		return nil
	}
	var out []flowStep
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	for i := range out {
		out[i].deps = parseDepends(out[i].DependsOn)
	}
	return out
}

// flowBranchOf reads a step's decision branches (nil for kinds that carry none).
func flowBranchOf(s flowStep) *flowBranches {
	if strings.TrimSpace(s.Config) == "" {
		return nil
	}
	var b flowBranches
	if err := json.Unmarshal([]byte(s.Config), &b); err != nil {
		return nil
	}
	if b.Success == "" && b.Loop == "" {
		return nil
	}
	return &b
}

// flowOrder topologically orders the steps (Kahn), keeping the AUTHORED order
// among steps that are simultaneously ready. A cycle (which the data should never
// contain) appends the remainder rather than dropping it: a renderer must not
// silently hide a node.
func flowOrder(steps []flowStep) []flowStep {
	byID := make(map[string]flowStep, len(steps))
	for _, s := range steps {
		byID[s.ID] = s
	}
	pending := make(map[string]bool, len(steps))
	for _, s := range steps {
		pending[s.ID] = true
	}
	out := make([]flowStep, 0, len(steps))
	for len(out) < len(steps) {
		progressed := false
		for _, s := range steps {
			if !pending[s.ID] {
				continue
			}
			ready := true
			for _, d := range s.deps {
				if _, known := byID[d]; !known {
					continue // a dep the version does not define cannot block a render
				}
				if pending[d] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, s)
				delete(pending, s.ID)
				progressed = true
			}
		}
		if !progressed {
			// Cycle: append the remaining steps in authored order so nothing is lost.
			for _, s := range steps {
				if pending[s.ID] {
					out = append(out, s)
					delete(pending, s.ID)
				}
			}
		}
	}
	return out
}

// flowKindMark is the glyph for a step kind (ADR step kinds).
func flowKindMark(kind string) string {
	switch strings.ToLower(kind) {
	case "task":
		return "●"
	case "approval":
		return "⏸"
	case "decision", "loop_decision":
		return "◇"
	case "parallel":
		return "⑃"
	case "recover":
		return "↺"
	case "work_item":
		return "▪"
	case "project":
		return "▫"
	case "end":
		return "⏹"
	}
	return "·"
}

// flowLegend is the one-line key for the glyphs.
func flowLegend() string {
	return "● task  ⏸ approval  ◇ decision  ⑃ parallel  ↺ recover  ▪ work item  ▫ project  ⏹ end  ↻ re-entry"
}

// renderWorkflowFlow renders the flow plus a header (no cursor). Returns "" for no
// steps, so the caller can say so honestly rather than drawing an empty frame.
func renderWorkflowFlow(stepsJSON string, width int) string {
	body, _ := renderWorkflowFlowView(stepsJSON, width, "")
	return body
}

// renderWorkflowFlowView renders the flow with a CURSOR marker on the selected
// step, and returns the step→line map so an editor can keep that step on screen.
//
// The map is why the marker is part of the RENDERED text rather than an overlay:
// the pane's viewport scrolls by line, so the offsets have to describe exactly
// what was drawn. A cursor drawn outside the text could not be tracked.
func renderWorkflowFlowView(stepsJSON string, width int, sel string) (string, map[string]int) {
	steps := parseFlowSteps(stepsJSON)
	if len(steps) == 0 {
		return "", nil
	}
	ordered := flowOrder(steps)
	// Names for branch targets, and the set of steps some branch jumps BACK to.
	nameOf := make(map[string]string, len(steps))
	for _, s := range steps {
		nameOf[s.ID] = s.Name
	}
	reentry := map[string]bool{}
	for _, s := range steps {
		if b := flowBranchOf(s); b != nil && b.Loop != "" {
			reentry[b.Loop] = true
		}
	}

	// The cursor/selection marker: the step the operator is on. Passing it in
	// keeps the marker in the RENDERED text, so the pane's line offsets (used for
	// cursor-follow scrolling) stay correct.
	cursorAt := -1
	if sel != "" {
		for i, s := range ordered {
			if s.ID == sel {
				cursorAt = i
				break
			}
		}
	}

	lineOffsets := make(map[string]int, len(ordered))
	var b strings.Builder
	line := 0
	write := func(s string) {
		b.WriteString(s + "\n")
		line++
	}
	write(theme.HintText.Render(flowLegend()))
	write("")
	for i, s := range ordered {
		lineOffsets[s.ID] = line // the line the step's NAME lands on
		mark := flowKindMark(s.Kind)
		name := s.Name
		if name == "" {
			name = s.ID
		}
		if reentry[s.ID] {
			name += " ↻"
		}
		idx := fmt.Sprintf("%2d ", i+1)
		cursor := "  "
		if i == cursorAt {
			cursor = theme.DetailKey.Render("▸") + " "
		}
		write(" " + cursor + theme.DetailKey.Render(idx) + mark + " " + name)

		// The dim meta line: kind, the worker/gate it names, and any fan-in.
		meta := []string{strings.ToLower(s.Kind)}
		// An approval's REVIEWER is operationally load-bearing — a human gate blocks
		// the run until someone acts; an automated one does not — so it earns a
		// place on the meta line when the config carries it.
		if br := flowBranchOf(s); br != nil && br.Reviewer != "" {
			meta = append(meta, "reviewer: "+br.Reviewer)
		}
		if s.Ref != "" {
			ref := s.Ref
			if s.WorkerVer > 0 {
				ref += " v" + screenkit.FmtInt(int(s.WorkerVer))
			}
			meta = append(meta, ref)
		}
		if s.GateRef != "" {
			meta = append(meta, "gate: "+s.GateRef)
		}
		if len(s.deps) > 1 {
			names := make([]string, 0, len(s.deps))
			for _, d := range s.deps {
				if n := nameOf[d]; n != "" {
					names = append(names, n)
				} else {
					names = append(names, d)
				}
			}
			meta = append(meta, "joins "+strings.Join(names, " + "))
		}
		write("    " + theme.HintText.Render(strings.Join(meta, " · ")))

		// Branches, NAMED (see the file header): the target may sit anywhere in
		// the flow, so a name is the only honest way to show where control goes.
		if br := flowBranchOf(s); br != nil {
			if br.Success != "" {
				write("    " + theme.HintText.Render("├ success → "+branchTarget(nameOf, br.Success)))
			}
			if br.Loop != "" {
				label := "└ loop ↻ " + branchTarget(nameOf, br.Loop)
				if br.MaxIter > 0 {
					label += fmt.Sprintf(" (max %d)", br.MaxIter)
				}
				write("    " + theme.HintText.Render(label))
			}
		}

		// Connector to the next step, unless this is the last or a terminal sink.
		if i < len(ordered)-1 && strings.ToLower(s.Kind) != "end" {
			write("    " + theme.HintText.Render("│"))
		}
	}
	return strings.TrimRight(b.String(), "\n"), lineOffsets
}

// branchTarget resolves a branch's step id to its name for display.
func branchTarget(nameOf map[string]string, id string) string {
	if n := nameOf[id]; n != "" {
		return n
	}
	return id
}

// flowStepCount is the parse count for the detail fields.
func flowStepCount(stepsJSON string) int { return len(parseFlowSteps(stepsJSON)) }

// pickFlowVersion chooses the version whose flow to show: the PUBLISHED one (what
// a run would execute), else the newest by version number (a workflow that has
// only ever been drafted).
func pickFlowVersion(versions []*apiv1.WorkflowVersion) *apiv1.WorkflowVersion {
	var newest *apiv1.WorkflowVersion
	for _, v := range versions {
		if v.GetStatus() == apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_PUBLISHED {
			return v
		}
		if newest == nil || v.GetVersion() > newest.GetVersion() {
			newest = v
		}
	}
	return newest
}
