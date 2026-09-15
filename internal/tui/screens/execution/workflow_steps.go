// workflow_steps.go — the workflow STEP editor: add, edit, remove, wire, branch.
//
// The shape, per the operator's direction: the FLOW view IS the editor. There is
// no separate canvas, because a step is operated on where it already lives — the
// rendered flow — and \`e\` edits whatever step the cursor is on.
//
// The kinds offered are exactly the ones the BACKEND can run, checked against the
// reconciler's kind switch rather than against the GUI's palette:
//
//	task           ref = worker (+ pinned worker_version); config.recovery
//	approval       config.reviewer, config.loop_branch, config.max_iterations
//	loop_decision  config.success_branch, config.loop_branch, config.max_iterations
//	parallel       NO config — its handler marks itself succeeded and DEPENDENTS fan out
//	end            NO config — terminal sink
//
// `decision` is deliberately absent: its reconciler case is a stub ("v0.1: default
// branch (true)") so a decision step always takes one path regardless of config,
// and it has zero live uses. Offering it would be offering a branch that does not
// branch. `policy` is absent because it is not a step kind at all — see ed856d99,
// which removed the GUI's invented one.
//
// WHAT EACH KIND'S CONFIG ACTUALLY CONTAINS is read off the reconciler, its only
// reader — and the answer is NOT symmetric, which is why this file no longer
// reduces the shapes to one "dual" flag:
//
//   - approvalConfig (workflow_reconciler.go:4892) has loop_branch and
//     max_iterations and NO success_branch FIELD AT ALL. An approval proceeds
//     forward through the DAG (depends_on); only rejection LOOP-BACKS
//     (workflow_reconciler.go:967). So the SUCCESS → field the first cut offered on
//     an approval wrote a key nothing reads. Live seed data does carry
//     success_branch on approval steps — the canvas authored it, because the canvas
//     draws that edge — so mergeStepConfig PRESERVES it, but it is not offered as an
//     editable field: editing a value with no consumer is not configuration.
//
//   - loopDecisionConfig (workflow_reconciler.go:4849) has BOTH, and the loop is
//     real (workflow_reconciler.go:2528). success_branch is read in exactly one
//     place — a hardcoded legacy special case for the terminal devops→end loop
//     (workflow_reconciler.go:2561) — so forward progress is the DAG here too; the
//     field is offered because the canvas authors it and the flow view DRAWS it,
//     not because the general path consults it.
//
//   - stepRecoveryConfig (workflow_reconciler.go:3885) is a task's config: strategy
//     and max_attempts. Those two are consumed
//     (workflow_reconciler.go:4659-4671, the switch whose cases are retry /
//     summarize_restart / human_escalation / stop). `retry_delay_seconds` is parsed
//     into the same struct and then NEVER read — the real delay is
//     recovery.defaultRetryDelaySeconds — so it is preserved, not offered.
//
//   - `conflict_value` and `exhausted_review` appear in every seeded loop_decision
//     config and in NO reader anywhere in the tree; `branch_from` is a declared but
//     unread field on loopDecisionConfig. All three are preserved by the merge and
//     offered nowhere — the previous cut invented labels ("branch taken on
//     conflict") for keys that do nothing.
//
// The APPROVER of a worker-backed approval lives in the step's `ref`, not in the
// config: workflow_reconciler.go:2718 says so outright ("The step's ref field
// carries the worker ID (same as TASK steps)") and falls back to config.worker_ref
// only for older workflows.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// stepKindOffer is the authoring vocabulary, in the order the picker shows it.
//
// Loop and Success are TWO INDEPENDENT questions — "does the reconciler re-enter a
// named step when this kind rejects?" and "does it carry a forward target?" — and
// the honest answer differs by kind. A single `Dual` flag conflated them and, for
// approval, claimed a success_branch the reconciler has no field for.
type stepKindOffer struct {
	Kind    string // the WIRE string the reconciler switches on
	Label   string
	Loop    bool // the reconciler RE-ENTERS a named step on rejection/failure
	Success bool // the step carries a success_branch
}

var stepKindsOffered = []stepKindOffer{
	{Kind: "task", Label: "worker (task) — dispatches a worker"},
	{Kind: "approval", Label: "approval — a gate; rejection loops back", Loop: true},
	{Kind: "loop_decision", Label: "loop — SUCCESS forward / LOOP back, needs max iterations", Loop: true, Success: true},
	{Kind: "parallel", Label: "parallel — fans out to whatever depends on it"},
	{Kind: "end", Label: "end — terminal sink"},
}

// stepKindOfferFor finds a kind's shape. ok=false for an unknown kind, which the
// forms treat as "offer nothing kind-specific" rather than guessing.
func stepKindOfferFor(kind string) (stepKindOffer, bool) {
	for _, o := range stepKindsOffered {
		if o.Kind == kind {
			return o, true
		}
	}
	return stepKindOffer{}, false
}

// stepKindHasLoop / stepKindHasSuccess read one half of the shape.
func stepKindHasLoop(kind string) bool {
	o, ok := stepKindOfferFor(kind)
	return ok && o.Loop
}

func stepKindHasSuccess(kind string) bool {
	o, ok := stepKindOfferFor(kind)
	return ok && o.Success
}

func stepKindOptions() []kit2.Option {
	out := make([]kit2.Option, 0, len(stepKindsOffered))
	for _, o := range stepKindsOffered {
		out = append(out, kit2.Option{Value: o.Kind, Label: o.Label})
	}
	return out
}

// targetOptions builds the step-target picker choices from THIS workflow's own
// steps.
//
// This is the piece that makes branch editing possible at all: success_branch and
// loop_branch are step IDs living inside a config JSON blob, and no human can type
// one. The VALUE is the id the wire needs; the LABEL is the name the operator
// reads — the same answer the model picker gives for model_ref.
func targetOptions(steps []flowStep, exclude string) []kit2.Option {
	out := make([]kit2.Option, 0, len(steps))
	for _, s := range steps {
		if s.ID == exclude {
			continue // a step cannot branch to itself
		}
		label := s.Name
		if label == "" {
			label = s.ID
		}
		label += "  (" + strings.ToLower(s.Kind) + ")"
		out = append(out, kit2.Option{Value: s.ID, Label: label})
	}
	return out
}

// --- step selection ---------------------------------------------------------

// flowStepsOf parses the DRAFT the editor targets (the version the pane shows),
// in flow order so the cursor walks what the operator sees.
func (m *Model) flowStepsOf() []flowStep {
	if m.stepSteps == "" {
		return nil
	}
	return flowOrder(parseFlowSteps(m.stepSteps))
}

// selectedFlowStep returns the step the editor cursor is on.
func (m *Model) selectedFlowStep() (flowStep, bool) {
	steps := m.flowStepsOf()
	if m.stepSel == "" {
		if len(steps) == 0 {
			return flowStep{}, false
		}
		return steps[0], true
	}
	for _, s := range steps {
		if s.ID == m.stepSel {
			return s, true
		}
	}
	return flowStep{}, false
}

// stepCursor moves the editor cursor by delta over the FLOW order and repaints.
func (m *Model) stepCursor(delta int) tea.Cmd {
	steps := m.flowStepsOf()
	if len(steps) == 0 {
		return nil
	}
	pos := 0
	for i, s := range steps {
		if s.ID == m.stepSel {
			pos = i
			break
		}
	}
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(steps) {
		pos = len(steps) - 1
	}
	m.stepSel = steps[pos].ID
	return m.paintFlow()
}

// --- serialisation ----------------------------------------------------------

// flowStepToWire converts a step back to the JSON shape the server stores. Every
// field the parser reads is written back, so a round trip through the editor never
// silently drops one.
func flowStepToWire(s flowStep) map[string]any {
	deps := s.deps
	if deps == nil {
		deps = []string{}
	}
	return map[string]any{
		"id":              s.ID,
		"name":            s.Name,
		"kind":            s.Kind,
		"ref":             s.Ref,
		"worker_version":  s.WorkerVer,
		"depends_on":      deps,
		"gate_policy_ref": s.GateRef,
		"config":          s.Config,
	}
}

// marshalSteps writes a step list back as the JSON array the server expects.
//
// Position fields are NOT emitted: they are presentation-only (nothing in the
// plane reads them), so writing them would be inventing data the GUI never needs
// from us and that we cannot compute meaningfully.
func marshalSteps(steps []flowStep) (string, error) {
	out := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		out = append(out, flowStepToWire(s))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// replaceStep swaps an edited step back into the raw step list, preserving the
// list's ORDER (the JSON order is the authored order, and flowOrder keeps it
// stable among ready steps, so reordering here would churn the display).
func replaceStep(steps []flowStep, edited flowStep) []flowStep {
	for i := range steps {
		if steps[i].ID == edited.ID {
			steps[i] = edited
			return steps
		}
	}
	return append(steps, edited)
}

// removeStep drops a step AND every reference to it, so the workflow cannot be
// saved with a dangling depends_on or a branch pointing at a step that no longer
// exists.
func removeStep(steps []flowStep, id string) []flowStep {
	out := make([]flowStep, 0, len(steps))
	for _, s := range steps {
		if s.ID == id {
			continue
		}
		kept := make([]string, 0, len(s.deps))
		for _, d := range s.deps {
			if d != id {
				kept = append(kept, d)
			}
		}
		s.deps = kept
		if b := flowBranchOf(s); b != nil {
			if b.Success == id {
				b.Success = ""
			}
			if b.Loop == id {
				b.Loop = ""
			}
			s.Config, _ = marshalBranches(s, b)
		}
		out = append(out, s)
	}
	return out
}

// marshalBranches rewrites a step's config with the given branch target changes,
// preserving every OTHER config key (recovery, conflict_value, exhausted_review …)
// — a step's config is more than its branches, and clobbering it would silently
// destroy settings the operator never touched.
func marshalBranches(s flowStep, b *flowBranches) (string, error) {
	raw := map[string]any{}
	if strings.TrimSpace(s.Config) != "" {
		_ = json.Unmarshal([]byte(s.Config), &raw)
	}
	if b.Success == "" {
		delete(raw, "success_branch")
	} else {
		raw["success_branch"] = b.Success
	}
	if b.Loop == "" {
		delete(raw, "loop_branch")
	} else {
		raw["loop_branch"] = b.Loop
	}
	if b.MaxIter > 0 {
		raw["max_iterations"] = b.MaxIter
	} else {
		delete(raw, "max_iterations")
	}
	if b.Reviewer != "" {
		raw["reviewer"] = b.Reviewer
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// nextStepID mints an id for a new step. The id is what depends_on and the branch
// fields reference, so it only has to be unique within the workflow — a short
// readable prefix plus a counter is enough and keeps the JSON legible.
func nextStepID(steps []flowStep) string {
	taken := map[string]bool{}
	for _, s := range steps {
		taken[s.ID] = true
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("step-new%d", i)
		if !taken[id] {
			return id
		}
	}
}

// --- forms ------------------------------------------------------------------

// editStepForm edits an existing step. The branch FIELDS are only offered when the
// kind is dual, which is the operator's "on loops, it should show a success/failure
// when you add a step or edit the loop step".
func (m *Model) editStepForm(s flowStep) *kit2.Form {
	steps := m.flowStepsOf()
	br := flowBranchOf(s)
	if br == nil {
		br = &flowBranches{}
	}
	specs := []kit2.FieldSpec{
		{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: s.Name},
		{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Initial: s.Kind, Options: stepKindOptions()},
	}
	// A worker id on the step is the TASK's dispatchee AND a worker-backed
	// APPROVAL's approver (workflow_reconciler.go:2718), so both kinds get the field.
	if s.Kind == "task" || s.Kind == "approval" {
		refLabel := "Worker id (blank = none)"
		if s.Kind == "approval" {
			refLabel = "Worker id — the APPROVER (only used when Reviewer = worker)"
		}
		specs = append(specs,
			kit2.FieldSpec{Name: "ref", Label: refLabel, Kind: kit2.KText, Initial: s.Ref,
				Placeholder: "w_se_senior_software_engineer"},
			kit2.FieldSpec{Name: "worker_version", Label: "Worker version (0 = latest)", Kind: kit2.KNumber,
				Initial: fmt.Sprintf("%d", s.WorkerVer)},
		)
	}
	// A TASK's config IS its recovery policy: strategy + max_attempts, the pair the
	// reconciler consumes (workflow_reconciler.go:4659-4671).
	if s.Kind == "task" {
		recovery := parseRecoveryConfig(s.Config)
		specs = append(specs,
			kit2.FieldSpec{Name: "recovery_strategy", Label: "On failure (recovery strategy)", Kind: kit2.KSelect,
				Initial: recovery.Strategy, Options: recoveryStrategyOptions()},
			kit2.FieldSpec{Name: "recovery_max_attempts", Label: "Recovery: max attempts before the step fails", Kind: kit2.KNumber,
				Initial: optIntString(recovery.MaxAttempts), Validate: validateNonNegativeInt,
				Placeholder: "built-in 3 when blank"},
		)
	}
	if s.Kind == "approval" {
		specs = append(specs, kit2.FieldSpec{Name: "reviewer", Label: "Reviewer", Kind: kit2.KSelect,
			Initial: orDefaultStr(br.Reviewer, "human"),
			Options: []kit2.Option{
				{Value: "human", Label: "human — blocks for a person"},
				{Value: "worker", Label: "worker — dispatches the Worker id above (needs it set)"},
			}})
	}
	// FORWARD. Offered only where the kind's config struct actually has the key —
	// approvalConfig has none, so an approval gets no SUCCESS → field.
	if stepKindHasSuccess(s.Kind) {
		specs = append(specs, kit2.FieldSpec{Name: "success_branch", Label: "SUCCESS → (step it continues to)", Kind: kit2.KPicker,
			Initial: br.Success, Options: targetOptions(steps, s.ID), Placeholder: "type to search this workflow's steps"})
	}
	// BACK — the rejection/failure path, which the reconciler really does follow.
	if stepKindHasLoop(s.Kind) {
		loopLabel, iterLabel := "LOOP → (step it re-enters on failure)", "Max iterations before the loop fails"
		if s.Kind == "approval" {
			loopLabel = "LOOP → (step it re-enters on rejection — BLANK = a rejection does nothing)"
			iterLabel = "Max rejections before the run fails"
		}
		specs = append(specs,
			kit2.FieldSpec{Name: "loop_branch", Label: loopLabel, Kind: kit2.KPicker,
				Initial: br.Loop, Options: targetOptions(steps, s.ID), Placeholder: "type to search this workflow's steps"},
			kit2.FieldSpec{Name: "max_iterations", Label: iterLabel, Kind: kit2.KNumber,
				Initial: fmt.Sprintf("%d", br.MaxIter), Validate: validatePositiveInt},
		)
	}
	// There is NO raw JSON config box. Every key these kinds consume has a field
	// above, and mergeStepConfig PRESERVES whatever the fields do not model — so the
	// box never bought anything except a way to corrupt a config by hand: "it still
	// just has me editing a JSON for the config".

	f := kit2.NewForm("Edit step: "+orDefaultStr(s.Name, s.ID), specs...)
	f.Focused = true
	f.Width = 70
	id := s.ID
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		edited := flowStep{
			ID:        id,
			Name:      strings.TrimSpace(v["name"]),
			Kind:      v["kind"],
			Ref:       strings.TrimSpace(v["ref"]),
			GateRef:   s.GateRef,
			deps:      s.deps,
			DependsOn: s.DependsOn,
		}
		if n, err := parseOptInt(v["worker_version"]); err == nil {
			edited.WorkerVer = n
		}
		// The config is MERGED, never replaced. A key the form does not show — a
		// legacy success_branch on an approval, a key this editor has not learned —
		// survives the edit instead of being silently deleted by a submit.
		cfg, err := mergeStepConfig(s.Config, configEdits(edited.Kind, s.Config, v))
		if err != nil {
			return nil, err
		}
		edited.Config = cfg
		if err := validateStepConfigForKind(edited); err != nil {
			return nil, err
		}
		return m.saveSteps(replaceStep(m.rawFlowSteps(), edited), "edit step "+edited.Name)
	}
	return f
}

// addStepForm creates a new step. `-` on the flow opens it. The fields cover every
// kind the picker offers, each labelled with the kind it belongs to, because the
// kind is chosen IN this form — a field cannot appear after the choice is made —
// and the vocabulary is small enough that the irrelevant ones are obvious. Every
// kind-specific requirement is checked on submit, at the field, rather than left for
// a run to fail on.
//
// There is no raw JSON config box: it was the operator's "it still just has me
// editing a JSON for the config".
func (m *Model) addStepForm() *kit2.Form {
	steps := m.flowStepsOf()
	// A new step defaults to running AFTER the current cursor, which is almost
	// always the intent when you add a step while looking at one.
	afterID := ""
	if cur, ok := m.selectedFlowStep(); ok {
		afterID = cur.ID
	}
	f := kit2.NewForm("Add step",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true,
			Placeholder: "QA Engineer"},
		kit2.FieldSpec{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Initial: "task", Options: stepKindOptions()},
		kit2.FieldSpec{Name: "runs_after", Label: "Runs after (blank = starts the flow)", Kind: kit2.KPicker,
			Initial: afterID, Options: targetOptions(steps, ""), Placeholder: "type to search this workflow's steps"},
		kit2.FieldSpec{Name: "ref", Label: "Worker id — worker steps, or a worker-backed approval", Kind: kit2.KText,
			Placeholder: "w_se_qa_engineer"},
		kit2.FieldSpec{Name: "recovery_strategy", Label: "On failure — worker steps", Kind: kit2.KSelect,
			Initial: "retry", Options: recoveryStrategyOptions()},
		kit2.FieldSpec{Name: "recovery_max_attempts", Label: "Recovery: max attempts — worker steps", Kind: kit2.KNumber,
			Initial: "3", Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "reviewer", Label: "Reviewer — approval steps", Kind: kit2.KSelect, Initial: "human",
			Options: []kit2.Option{
				{Value: "human", Label: "human — blocks for a person"},
				{Value: "worker", Label: "worker — dispatches the Worker id above"},
			}},
		kit2.FieldSpec{Name: "success_branch", Label: "SUCCESS → — loop steps", Kind: kit2.KPicker,
			Options: targetOptions(steps, ""), Placeholder: "type to search this workflow's steps"},
		kit2.FieldSpec{Name: "loop_branch", Label: "LOOP → — approval + loop steps", Kind: kit2.KPicker,
			Options: targetOptions(steps, ""), Placeholder: "type to search this workflow's steps"},
		kit2.FieldSpec{Name: "max_iterations", Label: "Max iterations/rejections — approval + loop", Kind: kit2.KNumber,
			Validate: validatePositiveInt, Placeholder: "3"},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		raw := m.rawFlowSteps()
		kind := v["kind"]
		newID := nextStepID(raw)
		step := flowStep{
			ID:   newID,
			Name: strings.TrimSpace(v["name"]),
			Kind: kind,
			Ref:  strings.TrimSpace(v["ref"]),
		}
		if after := strings.TrimSpace(v["runs_after"]); after != "" {
			step.deps = []string{after}
		}
		// A new step starts from an EMPTY config, so only the keys its kind's fields
		// produced are written — no inherited keys, no raw JSON.
		cfg, err := mergeStepConfig("", configEdits(kind, "", v))
		if err != nil {
			return nil, err
		}
		step.Config = cfg
		if err := validateStepConfigForKind(step); err != nil {
			return nil, err
		}
		return m.saveSteps(append(raw, step), "add step "+step.Name)
	}
	return f
}

// validateStepConfigForKind catches the shapes the reconciler would reject — or,
// worse, SILENTLY IGNORE — at the field rather than in a run.
func validateStepConfigForKind(s flowStep) error {
	offer, _ := stepKindOfferFor(s.Kind)
	switch s.Kind {
	case "task":
		if s.Ref == "" {
			return errors.New("a worker step needs a worker id")
		}
	case "approval":
		// A worker-backed approval with no ref FAILS the step at run time
		// (workflow_reconciler.go:2729 — "worker-backed approval step %s has no
		// worker ref"), so refuse it at the field.
		if approvalReviewer(s.Config) == "worker" && s.Ref == "" {
			return errors.New("a worker-backed approval needs a worker id (or set Reviewer to human)")
		}
	case "loop_decision":
		// The reconciler FAILS a loop decision whose loop_branch is empty or whose
		// max_iterations is < 1 (workflow_reconciler.go:2359 — "missing or invalid
		// config (loop_branch=%q, max_iterations=%d)").
		b := flowBranchOf(s)
		if b == nil || b.Loop == "" || b.MaxIter < 1 {
			return errors.New("a loop step needs a LOOP → target and max iterations >= 1")
		}
	}
	// A LOOP target with no bound can never re-enter: the reconciler compares the
	// iteration count against max_iterations, so 0 makes the loop dead on arrival.
	// (An approval with a BLANK loop_branch is allowed — the reconciler simply skips
	// the loop-back, which makes the gate one-shot — and the field says so.)
	if offer.Loop {
		if b := flowBranchOf(s); b != nil && b.Loop != "" && b.MaxIter < 1 {
			return errors.New("a step with a LOOP → target needs max iterations >= 1 (otherwise it can never re-enter)")
		}
	}
	return nil
}

// approvalReviewer reads the reviewer out of a step's config. Absent means the human
// path: the reconciler acts on the value only when it is exactly "worker"
// (workflow_reconciler.go:2712), and parseApprovalConfig applies no default.
func approvalReviewer(config string) string {
	var raw struct {
		Reviewer string `json:"reviewer"`
	}
	if strings.TrimSpace(config) != "" {
		_ = json.Unmarshal([]byte(config), &raw)
	}
	return raw.Reviewer
}

// --- config MERGE -------------------------------------------------------------

// configEdits turns a form's values into config KEY EDITS for a step of the given
// kind. Only the keys the reconciler READS for that kind appear here; every other key
// in the stored config is left alone by mergeStepConfig.
//
// `existing` is the step's current config, needed because a task's recovery policy is
// a NESTED object: merging it as a whole would drop the keys inside it that this
// editor does not model (retry_delay_seconds).
//
// A blank value deletes its key, because an empty branch id or an empty reviewer is
// not a value — it is the ABSENCE of one, and writing "" would make the JSON claim a
// setting that is not there.
func configEdits(kind, existing string, v map[string]string) map[string]any {
	edits := map[string]any{}
	if kind == "task" {
		edits["recovery"] = recoveryBlock(existingRecovery(existing), v["recovery_strategy"], v["recovery_max_attempts"])
	}
	if kind == "approval" {
		edits["reviewer"] = strings.TrimSpace(v["reviewer"])
	}
	if stepKindHasSuccess(kind) {
		edits["success_branch"] = strings.TrimSpace(v["success_branch"])
	}
	if stepKindHasLoop(kind) {
		edits["loop_branch"] = strings.TrimSpace(v["loop_branch"])
		edits["max_iterations"] = intOrNil(v["max_iterations"])
	}
	return edits
}

// existingRecovery pulls the stored `recovery` object out as a raw map, so an edit
// merges INTO it instead of replacing it. Empty when there is none, or when the config
// is unparseable — in which case mergeStepConfig refuses the write anyway.
func existingRecovery(config string) map[string]any {
	var outer struct {
		Recovery map[string]any `json:"recovery"`
	}
	if strings.TrimSpace(config) != "" {
		_ = json.Unmarshal([]byte(config), &outer)
	}
	if outer.Recovery == nil {
		return map[string]any{}
	}
	return outer.Recovery
}

// recoveryBlock merges the two form values into the STORED recovery object, so keys
// this editor does not model survive the edit. `retry_delay_seconds` is the concrete
// case: the reconciler parses it into stepRecoveryConfig, nothing consumes it, and a
// change to the strategy must not quietly delete it.
//
// The strategy falls back to "retry" — the reconciler's own default
// (readStepRecoveryConfig) and the one the GUI's step editor seeds — so a step always
// ends up carrying the policy that will actually run rather than a block the reader
// has to guess at.
func recoveryBlock(existing map[string]any, strategy, maxAttempts string) map[string]any {
	out := make(map[string]any, len(existing)+2)
	for k, v := range existing {
		out[k] = v
	}
	out["strategy"] = orDefaultStr(strategy, "retry")
	if n, err := parseOptInt(maxAttempts); err == nil && n > 0 {
		out["max_attempts"] = n
	}
	return out
}

// intOrNil parses a whole number, returning nil for blank or non-positive so the key
// is DELETED rather than written as 0 — the reconciler treats max_iterations < 1 as
// invalid, so 0 is a bound that can never be satisfied.
func intOrNil(raw string) any {
	n, err := parseOptInt(raw)
	if err != nil || n <= 0 {
		return nil
	}
	return n
}

// mergeStepConfig rewrites a step's config with the given key edits, PRESERVING every
// key it does not name.
//
// A step's config is more than one thing. A task carries its recovery policy; an
// approval carries a reviewer and a loop; a loop decision carries branches; and live
// data carries keys no version of this editor models (success_branch on an approval,
// conflict_value, exhausted_review). REPLACING the config with whatever the form
// happens to show would silently destroy all of it — which is precisely what a submit
// that writes an empty `config` does, and why the form no longer has that field.
//
// A nil value, or an empty string, DELETES the key: an absent key is how the wire
// says "not set", and the reconciler's own defaults then apply.
func mergeStepConfig(config string, edits map[string]any) (string, error) {
	raw := map[string]any{}
	if t := strings.TrimSpace(config); t != "" {
		if err := json.Unmarshal([]byte(t), &raw); err != nil {
			// Refuse rather than discard. Overwriting an unparseable config would
			// destroy whatever it holds, and it stays readable in the version's raw
			// steps for whoever needs to repair it.
			return "", errors.New("this step's config is not valid JSON, so it cannot be safely edited")
		}
	}
	for k, val := range edits {
		switch t := val.(type) {
		case nil:
			delete(raw, k)
		case string:
			if t == "" {
				delete(raw, k)
			} else {
				raw[k] = t
			}
		default:
			raw[k] = val
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// stepRecovery is a task step's recovery policy as the reconciler reads it.
type stepRecovery struct {
	Strategy    string
	MaxAttempts int
}

// parseRecoveryConfig reads a task step's recovery block the way the reconciler does
// — DEFAULTS INCLUDED — so the form shows the policy that will actually run rather
// than a blank slate. Mirrors readStepRecoveryConfig (workflow_reconciler.go:3897):
// an absent strategy means "retry", and a max_attempts <= 0 means 3.
func parseRecoveryConfig(config string) stepRecovery {
	rec := stepRecovery{Strategy: "retry", MaxAttempts: 3}
	if strings.TrimSpace(config) == "" {
		return rec
	}
	var outer struct {
		Recovery struct {
			Strategy    string `json:"strategy"`
			MaxAttempts int    `json:"max_attempts"`
		} `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(config), &outer); err != nil {
		return rec
	}
	if outer.Recovery.Strategy != "" {
		rec.Strategy = outer.Recovery.Strategy
	}
	if outer.Recovery.MaxAttempts > 0 {
		rec.MaxAttempts = outer.Recovery.MaxAttempts
	}
	return rec
}

// recoveryStrategyOptions is the strategy vocabulary the reconciler routes on — the
// four cases of the switch at workflow_reconciler.go:4671-4774 (retry / "",
// summarize_restart, human_escalation, stop) — with the same four values the GUI's
// step editor offers.
func recoveryStrategyOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "retry", Label: "retry — blind retry: a fresh work item, re-dispatched (default)"},
		{Value: "summarize_restart", Label: "summarize_restart — capture, summarize and resume with context"},
		{Value: "human_escalation", Label: "human_escalation — park at approval_pending for a person"},
		{Value: "stop", Label: "stop — permanent failure, no retry"},
	}
}

// optIntString renders an int for a field initial (blank is reserved for "unset").
func optIntString(v int) string { return fmt.Sprintf("%d", v) }

// validateNonNegativeInt accepts blank or a run of digits. It exists next to
// validatePositiveInt because the two fields mean different things: a recovery
// attempt count of 0 is a real value ("do not retry"), while a loop bound of 0 is
// invalid.
func validateNonNegativeInt(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return errors.New("must be a whole number")
		}
	}
	return nil
}

// --- write ------------------------------------------------------------------

// rawFlowSteps returns the step list in its AUTHORED order (not flow order): an
// edit must not silently reorder the JSON the server stores.
func (m *Model) rawFlowSteps() []flowStep { return parseFlowSteps(m.stepSteps) }

// stepEditorTarget resolves the DRAFT this editor writes to, creating one when the
// workflow has none.
//
// Published versions are IMMUTABLE — the server rejects an edit to anything but
// the latest draft ("latest version (v%d) is not draft; published versions are
// immutable") — so step editing implies a draft, exactly as the worker version
// editor already does. Reusing that path avoids a second, divergent mechanism.
func (m *Model) stepEditorTarget() (workflowID, versionID string, steps string, err error) {
	if m.stepWorkflowID == "" {
		return "", "", "", errors.New("no workflow open")
	}
	return m.stepWorkflowID, m.stepVersionID, m.stepSteps, nil
}

// saveSteps persists an edited step list to the draft and repaints the flow.
func (m *Model) saveSteps(steps []flowStep, what string) (tea.Cmd, error) {
	workflowID, _, _, err := m.stepEditorTarget()
	if err != nil {
		return nil, err
	}
	// Refuse to save a workflow with dangling references. The server validates the
	// shape but NOT the graph, so a depends_on naming a deleted step would be
	// accepted and then simply never become ready — a workflow that silently does
	// not run.
	if err := validateStepGraph(steps); err != nil {
		return nil, err
	}
	blob, err := marshalSteps(steps)
	if err != nil {
		return nil, err
	}
	m.stepSteps = blob
	id := workflowID
	return m.Mutate(mutate.Request{
		Name: what, Source: srcWorkflows,
		Do: func(ctx context.Context) error { return m.rpcUpdateSteps(ctx, id, blob) },
	}), nil
}

// validateStepGraph enforces the invariants the server does not: ids unique, every
// reference resolvable, and every dual kind actually carrying its targets.
func validateStepGraph(steps []flowStep) error {
	ids := map[string]bool{}
	for _, s := range steps {
		if strings.TrimSpace(s.ID) == "" {
			return errors.New("a step has no id")
		}
		if ids[s.ID] {
			return errors.New("duplicate step id " + s.ID)
		}
		ids[s.ID] = true
	}
	for _, s := range steps {
		for _, d := range s.deps {
			if !ids[d] {
				return errors.New("step " + orDefaultStr(s.Name, s.ID) + " runs after unknown step " + d)
			}
		}
		if b := flowBranchOf(s); b != nil {
			if b.Success != "" && !ids[b.Success] {
				return errors.New("step " + orDefaultStr(s.Name, s.ID) + " SUCCESS → unknown step " + b.Success)
			}
			if b.Loop != "" && !ids[b.Loop] {
				return errors.New("step " + orDefaultStr(s.Name, s.ID) + " LOOP → unknown step " + b.Loop)
			}
			if b.Loop != "" && b.MaxIter < 1 {
				return errors.New("step " + orDefaultStr(s.Name, s.ID) + " loops but has no max iterations")
			}
		}
	}
	return nil
}

// --- paint ------------------------------------------------------------------

// enterStepEditor points the editor at the draft the pane is already showing and
// seeds the cursor on the first step.
func (m *Model) enterStepEditor(workflowID, name string, v *apiv1.WorkflowVersion) tea.Cmd {
	m.stepWorkflowID, m.stepWorkflowName = workflowID, name
	m.stepVersionID, m.stepSteps = "", ""
	if v != nil {
		m.stepVersionID, m.stepSteps = v.GetId(), v.GetSteps()
	}
	steps := m.flowStepsOf()
	m.stepSel = ""
	if len(steps) > 0 {
		m.stepSel = steps[0].ID
	}
	return m.paintFlow()
}

// paintFlow repaints the detail pane with the flow and its cursor marker, and
// follows the cursor.
//
// CURSOR-FOLLOW is the reason this exists rather than reusing the read-only path:
// a real workflow's flow is taller than the pane (SDLC (Human) renders 44 lines of
// flow alone), so selecting a step near the end has to SCROLL it into view or the
// operator edits something they cannot see.
func (m *Model) paintFlow() tea.Cmd {
	if m.stepWorkflowID == "" {
		return nil
	}
	body, offsets := renderWorkflowFlowView(m.stepSteps, m.w, m.stepSel)
	title := "Workflow: " + m.stepWorkflowName
	fields := []screenkit.Field{
		{Key: "workflow", Value: m.stepWorkflowID},
		{Key: "steps", Value: screenkit.FmtInt(len(m.flowStepsOf()))},
		{Key: "editing", Value: m.stepEditingLabel()},
	}
	if body == "" {
		body = theme.HintText.Render("  no steps yet — press - to add the first one")
	}
	m.Base.SetDetailContent(title, fields, body)

	// Follow the cursor: put its step a few lines below the top so the step's own
	// meta and branch lines are visible with it.
	if line, ok := offsets[m.stepSel]; ok {
		scroll := line - 2
		if scroll < 0 {
			scroll = 0
		}
		m.Base.SetDetailScrollTop(scroll)
	}
	return nil
}

// stepEditingLabel names what an edit will write to: a draft, or the fact that
// saving creates one.
func (m *Model) stepEditingLabel() string {
	if m.stepVersionID == "" {
		return "draft v-n/a (no version yet — saving creates one)"
	}
	return m.stepVersionID
}

// stepDepsOf returns the cursor step's dependencies, for the header.
func (m *Model) stepDepsOf() []string {
	if s, ok := m.selectedFlowStep(); ok {
		return s.deps
	}
	return nil
}

// --- write RPC --------------------------------------------------------------

// rpcUpdateSteps persists an edited step list. When the target version is not a
// DRAFT it creates one first, because published versions are immutable — the server
// rejects an edit to anything else with FailedPrecondition ("latest version (v%d)
// is not draft; published versions are immutable"), and "edit a step" should not
// fail with a lecture about versioning. This mirrors the worker version editor,
// which creates a draft when none exists rather than refusing.
func (m *Model) rpcUpdateSteps(ctx context.Context, workflowID, steps string) error {
	if u := m.rpcUpdateWorkflowVersion; u != nil {
		if err := u(ctx, workflowID, steps); err == nil {
			return nil
		}
		// A failure here is usually "not draft". Create one and retry once, so a
		// published workflow is editable without the operator knowing the rule.
		if err := m.ensureDraft(ctx, workflowID); err != nil {
			return err
		}
		return u(ctx, workflowID, steps)
	}
	return errors.New("no workflow client")
}

// ensureDraft creates a draft version for the workflow when none is latest.
func (m *Model) ensureDraft(ctx context.Context, workflowID string) error {
	if m.rpcCreateWorkflowVersion == nil {
		return errors.New("no workflow client")
	}
	return m.rpcCreateWorkflowVersion(ctx, workflowID)
}

func (m *Model) defaultCreateWorkflowVersion(ctx context.Context, workflowID string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.CreateWorkflowVersion(ctx, connect.NewRequest(&apiv1.CreateWorkflowVersionRequest{
		WorkflowId:  workflowID,
		VersionNote: "draft created by the TUI step editor",
	}))
	return err
}

func (m *Model) defaultUpdateWorkflowVersion(ctx context.Context, workflowID, steps string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.UpdateWorkflowVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkflowVersionRequest{
		WorkflowId: workflowID,
		Steps:      steps,
	}))
	return err
}

// small helpers ---------------------------------------------------------------

func orDefaultStr(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func parseOptInt(raw string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	var n int32
	_, err := fmt.Sscanf(raw, "%d", &n)
	return n, err
}

func validatePositiveInt(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return errors.New("must be a whole number")
		}
	}
	return nil
}

// sortedStepIDs is used by tests and diagnostics to compare step sets without
// depending on map iteration order.
func sortedStepIDs(steps []flowStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}
