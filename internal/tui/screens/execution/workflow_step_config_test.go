package execution

// workflow_step_config_test.go — the step editor's CONFIG handling.
//
// The operator's complaint was "workflow edit is wrong. It still just has me editing
// a JSON for the config. We were supposed to implement that flow view as the editor."
//
// The flow view WAS the editor; the problem was that the step form still exposed a
// `Config (JSON — extra keys)` box and, on submit, wrote it back wholesale. That is
// not merely ugly — with the box empty it wrote `""` over a task's recovery policy,
// so renaming a step could silently destroy the policy that governs its failures.
// These tests pin the two halves of the fix: no JSON box, and a config that is
// MERGED key-by-key so nothing the form does not model is lost.

import (
	"context"
	"encoding/json"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// cfgEditorModel builds a model pointed at a two-step draft and captures every steps
// blob the editor saves.
func cfgEditorModel(t *testing.T, stepsJSON string) (*Model, *[]string) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	saved := &[]string{}
	m.rpcUpdateWorkflowVersion = func(_ context.Context, _ /*id*/, steps string) error {
		*saved = append(*saved, steps)
		return nil
	}
	m.rpcCreateWorkflowVersion = func(context.Context, string) error { return nil }
	m.enterStepEditor("wf-1", "Probe", &apiv1.WorkflowVersion{Id: "ver-1", Steps: stepsJSON})
	return m, saved
}

// fieldsOf collects a form's field kinds by name.
func fieldsOf(f *kit2.Form) map[string]kit2.Kind {
	out := map[string]kit2.Kind{}
	for _, s := range f.Specs {
		out[s.Name] = s.Kind
	}
	return out
}

func hasField(f *kit2.Form, name string) bool {
	for _, s := range f.Specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// --- no raw JSON anywhere in the step editor ---------------------------------

// THE core complaint. No kind's edit form may offer a JSON box: every key these kinds
// consume has a real field, and the merge preserves the rest.
func TestStepEditorOffersNoRawJSONConfigForAnyKind(t *testing.T) {
	const steps = `[{"id":"a","name":"A","kind":"task","ref":"w_x","depends_on":[]},
	                {"id":"b","name":"B","kind":"approval","config":"{\"reviewer\":\"human\"}","depends_on":["a"]},
	                {"id":"c","name":"C","kind":"loop_decision","config":"{\"loop_branch\":\"a\",\"max_iterations\":3}","depends_on":["b"]},
	                {"id":"d","name":"D","kind":"parallel","depends_on":["c"]},
	                {"id":"e","name":"E","kind":"end","config":"{}","depends_on":["d"]}]`
	m, _ := cfgEditorModel(t, steps)
	for _, s := range m.rawFlowSteps() {
		f := m.editStepForm(s)
		if hasField(f, "config") {
			t.Errorf("kind %q still offers a config field", s.Kind)
		}
		for name, kind := range fieldsOf(f) {
			if kind == kit2.KJSON {
				t.Errorf("kind %q offers raw JSON field %q — the operator's complaint", s.Kind, name)
			}
		}
	}
	// And the ADD form, which must not smuggle one back in.
	if hasField(m.addStepForm(), "config") {
		t.Error("the add form offers a config field")
	}
	for name, kind := range fieldsOf(m.addStepForm()) {
		if kind == kit2.KJSON {
			t.Errorf("the add form offers raw JSON field %q", name)
		}
	}
}

// --- the field set follows the KIND's config struct ---------------------------

// approvalConfig has no success_branch; loopDecisionConfig has both. The form must
// agree with the reconciler rather than with a single "dual" flag.
func TestStepFieldsFollowTheKindsRealConfigKeys(t *testing.T) {
	const steps = `[{"id":"a","name":"A","kind":"task","ref":"w_x","depends_on":[]},
	                {"id":"b","name":"B","kind":"approval","config":"{\"reviewer\":\"human\"}","depends_on":["a"]},
	                {"id":"c","name":"C","kind":"loop_decision","config":"{\"loop_branch\":\"a\",\"max_iterations\":3}","depends_on":["b"]},
	                {"id":"d","name":"D","kind":"end","config":"{}","depends_on":["c"]}]`
	m, _ := cfgEditorModel(t, steps)

	task := fieldsOf(m.editStepForm(flowStep{ID: "a", Name: "A", Kind: "task", Ref: "w_x"}))
	if _, ok := task["recovery_strategy"]; !ok {
		t.Error("a task must expose its recovery strategy — that key IS its config")
	}
	if _, ok := task["recovery_max_attempts"]; !ok {
		t.Error("a task must expose recovery max_attempts")
	}
	// retry_delay_seconds is parsed into stepRecoveryConfig and never read, so it is
	// NOT offered: a field that changes nothing is worse than no field.
	if _, ok := task["recovery_delay_seconds"]; ok {
		t.Error("retry_delay_seconds is parsed but never consumed — it must not be offered as configuration")
	}
	if _, ok := task["success_branch"]; ok {
		t.Error("a task has no branch targets")
	}

	approval := fieldsOf(m.editStepForm(flowStep{ID: "b", Name: "B", Kind: "approval"}))
	if _, ok := approval["reviewer"]; !ok {
		t.Error("an approval must expose its reviewer")
	}
	if _, ok := approval["loop_branch"]; !ok {
		t.Error("an approval loops back on rejection — it must expose a LOOP target")
	}
	if _, ok := approval["success_branch"]; ok {
		t.Error("approvalConfig has NO success_branch field — offering one writes a key nothing reads")
	}
	// The approver's worker id lives on the step's ref, not in the config
	// (workflow_reconciler.go:2718).
	if _, ok := approval["ref"]; !ok {
		t.Error("an approval must expose the approver worker — it is the step's ref")
	}

	loop := fieldsOf(m.editStepForm(flowStep{ID: "c", Name: "C", Kind: "loop_decision"}))
	if _, ok := loop["loop_branch"]; !ok {
		t.Error("a loop must expose its LOOP target")
	}
	if _, ok := loop["success_branch"]; !ok {
		t.Error("a loop must expose its SUCCESS target")
	}
	// on_missing_decision is a loopDecisionConfig key, and it is the replacement for
	// the engine's tenant-id hardcode — so a loop MUST be able to set it.
	if _, ok := loop["on_missing_decision"]; !ok {
		t.Error("a loop must expose on_missing_decision — it is how the operator says whether a missing verdict means re-ask, proceed, or fail")
	}
	// max_reask is a real, honoured bound (the re-ask loop fails at reaskCount >=
	// cfg.MaxReask), and it is the other half of the same question, so it belongs
	// beside the policy.
	if _, ok := loop["max_reask"]; !ok {
		t.Error("a loop must expose max_reask — it is the budget the missing-decision policy spends")
	}
	// THE PLATFORM CONTRACT MUST STAY OUT OF THE FORM. success_value/failure_value are
	// what every seeded worker prompt emits (`ORCHICON WORKER SUMMARY: success`), and
	// decision_field is not even consulted on the primary path. Exposing them would let
	// an operator point a gate at a word no worker emits, making every verdict miss and
	// every gate fail — silently, and only at run time.
	for _, contract := range []string{"decision_field", "success_value", "failure_value"} {
		if _, ok := loop[contract]; ok {
			t.Errorf("%s is platform contract, not operator configuration — exposing it lets a gate be pointed at a verdict no worker emits", contract)
		}
	}
	// Keys the previous cut invented labels for, none of which any reader consumes.
	for _, invented := range []string{"conflict_value", "exhausted_review"} {
		if _, ok := loop[invented]; ok {
			t.Errorf("%s appears in seed data and in no reader — it must not be offered as configuration", invented)
		}
	}

	end := fieldsOf(m.editStepForm(flowStep{ID: "d", Name: "D", Kind: "end"}))
	for _, unwanted := range []string{"recovery_strategy", "loop_branch", "success_branch", "reviewer"} {
		if _, ok := end[unwanted]; ok {
			t.Errorf("an end step carries no config — it must not expose %q", unwanted)
		}
	}
	// An approval has no missing-verdict problem (it proceeds unless explicitly
	// REJECTED), and approvalConfig has no such key — so it must not be offered there
	// any more than on a task.
	for _, kind := range []flowStep{{ID: "a", Name: "A", Kind: "task", Ref: "w_x"}, {ID: "b", Name: "B", Kind: "approval"}} {
		for _, unwanted := range []string{"on_missing_decision", "max_reask"} {
			if _, ok := fieldsOf(m.editStepForm(kind))[unwanted]; ok {
				t.Errorf("kind %q must not expose %q — it is a loopDecisionConfig key", kind.Kind, unwanted)
			}
		}
	}
}

// --- editing MERGES, it does not replace --------------------------------------

// The bug the JSON box caused: an edit that the form does not model — and, with the
// box empty, an edit to ANY field — rewrote the config. A task's recovery policy has
// to survive a rename.
func TestEditingATaskStepKeepsItsRecoveryAndUnknownKeys(t *testing.T) {
	const steps = `[{"id":"a","name":"Architect","kind":"task","ref":"w_x","config":` +
		`"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6,\"retry_delay_seconds\":30},\"future_key\":{\"nested\":true}}","depends_on":[]}]`
	m, saved := cfgEditorModel(t, steps)

	f := m.editStepForm(m.rawFlowSteps()[0])
	f.Set("name", "Architect (renamed)")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(*saved) != 1 {
		t.Fatalf("saves = %d, want 1", len(*saved))
	}
	after := parseFlowSteps((*saved)[0])
	if len(after) != 1 {
		t.Fatalf("saved %d steps, want 1", len(after))
	}
	if after[0].Name != "Architect (renamed)" {
		t.Errorf("name = %q, want the edit applied", after[0].Name)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(after[0].Config), &got); err != nil {
		t.Fatalf("saved config is not JSON: %v (%s)", err, after[0].Config)
	}
	rec, ok := got["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("the recovery policy was DESTROYED by an unrelated edit: %s", after[0].Config)
	}
	if rec["strategy"] != "summarize_restart" {
		t.Errorf("recovery.strategy = %v, want the stored value untouched", rec["strategy"])
	}
	if rec["max_attempts"].(float64) != 6 {
		t.Errorf("recovery.max_attempts = %v, want 6", rec["max_attempts"])
	}
	// A key the editor does not model is preserved, not dropped.
	if _, ok := got["future_key"]; !ok {
		t.Errorf("an unmodelled key was dropped by an edit: %s", after[0].Config)
	}
}

// Changing the recovery strategy through the form actually lands, and lands ALONGSIDE
// the keys the form does not show.
func TestRecoveryStrategyEditsMergeWithTheRest(t *testing.T) {
	const steps = `[{"id":"a","name":"A","kind":"task","ref":"w_x","config":"{\"recovery\":{\"strategy\":\"retry\",\"max_attempts\":2,\"retry_delay_seconds\":30},\"note\":\"keep me\"}","depends_on":[]}]`
	m, saved := cfgEditorModel(t, steps)

	f := m.editStepForm(m.rawFlowSteps()[0])
	f.Set("recovery_strategy", "human_escalation")
	f.Set("recovery_max_attempts", "9")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	var got map[string]any
	if err := json.Unmarshal([]byte(parseFlowSteps((*saved)[0])[0].Config), &got); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	rec := got["recovery"].(map[string]any)
	if rec["strategy"] != "human_escalation" || rec["max_attempts"].(float64) != 9 {
		t.Errorf("recovery not updated: %v", rec)
	}
	// retry_delay_seconds is not modelled by the form, so it must ride along.
	if rec["retry_delay_seconds"].(float64) != 30 {
		t.Errorf("the unmodelled retry_delay_seconds was dropped: %v", rec)
	}
	if got["note"] != "keep me" {
		t.Errorf("a sibling key was dropped: %v", got)
	}
}

// An approval carries a legacy success_branch the reconciler never reads, and the
// form does not offer it. Editing the approval must not delete it.
func TestApprovalsLegacySuccessBranchSurvivesAnEdit(t *testing.T) {
	const steps = `[{"id":"a","name":"Approval","kind":"approval","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-start\"}","depends_on":["step-start"]},
	                {"id":"step-sse","name":"SSE","kind":"task","ref":"w_x","depends_on":["a"]},
	                {"id":"step-start","name":"Start","kind":"task","ref":"w_x","depends_on":[]}]`
	m, saved := cfgEditorModel(t, steps)
	f := m.editStepForm(m.rawFlowSteps()[0])
	f.Set("name", "Approval (renamed)")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	a := parseFlowSteps((*saved)[0])[0]
	var got map[string]any
	if err := json.Unmarshal([]byte(a.Config), &got); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if got["success_branch"] != "step-sse" {
		t.Errorf("the legacy success_branch was dropped by an unrelated edit: %s", a.Config)
	}
	if got["loop_branch"] != "step-start" || got["reviewer"] != "human" {
		t.Errorf("an approval's real keys were lost: %s", a.Config)
	}
}

// An unparseable config is REFUSED rather than silently overwritten — overwriting it
// would destroy whatever it holds.
func TestUnparseableConfigIsRefusedNotDiscarded(t *testing.T) {
	if _, err := mergeStepConfig("{not json", map[string]any{"name": "x"}); err == nil {
		t.Fatal("an unparseable config must be refused, not replaced")
	}
	// And through the form: the submit returns the refusal rather than a write.
	m, saved := cfgEditorModel(t, `[{"id":"a","name":"A","kind":"task","ref":"w_x","config":"{broken","depends_on":[]}]`)
	f := m.editStepForm(m.rawFlowSteps()[0])
	if _, err := f.OnSubmit(f.Values, nil); err == nil {
		t.Fatal("the form submitted despite an unparseable config")
	}
	if len(*saved) != 0 {
		t.Fatalf("the editor saved %v despite an unparseable config", *saved)
	}
}

// Clearing a branch REMOVES the key rather than writing an empty id: the reconciler
// reads an absent loop_branch as "no loop", while "" would claim one.
func TestClearingABranchDeletesTheKey(t *testing.T) {
	out, err := mergeStepConfig(
		`{"loop_branch":"a","max_iterations":3,"reviewer":"human"}`,
		map[string]any{"loop_branch": "", "max_iterations": nil},
	)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if _, ok := got["loop_branch"]; ok {
		t.Errorf("a cleared branch was written as an empty id: %s", out)
	}
	if _, ok := got["max_iterations"]; ok {
		t.Errorf("a cleared bound was written as 0, which can never be satisfied: %s", out)
	}
	if got["reviewer"] != "human" {
		t.Errorf("an untouched key was lost: %s", out)
	}
}

// --- defaults and validation --------------------------------------------------

// The form shows the policy the reconciler would ACTUALLY apply, defaults included —
// otherwise "blank" would look like "unset" when the engine means "retry, 3".
func TestRecoveryDefaultsMirrorTheReconciler(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want stepRecovery
	}{
		{"no config at all", "", stepRecovery{Strategy: "retry", MaxAttempts: 3}},
		{"empty object", "{}", stepRecovery{Strategy: "retry", MaxAttempts: 3}},
		{"explicit", `{"recovery":{"strategy":"stop","max_attempts":1}}`, stepRecovery{Strategy: "stop", MaxAttempts: 1}},
		// readStepRecoveryConfig ignores a non-positive bound and keeps 3.
		{"zero bound falls back", `{"recovery":{"strategy":"stop","max_attempts":0}}`, stepRecovery{Strategy: "stop", MaxAttempts: 3}},
		{"broken json falls back", "{oops", stepRecovery{Strategy: "retry", MaxAttempts: 3}},
	} {
		if got := parseRecoveryConfig(tc.cfg); got != tc.want {
			t.Errorf("%s: parseRecoveryConfig(%q) = %+v, want %+v", tc.name, tc.cfg, got, tc.want)
		}
	}
}

// A loop that the reconciler would fail at run time is refused at the field.
func TestLoopStepsAreValidatedAtTheField(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		bad  bool
	}{
		{"no loop target", `{"max_iterations":3}`, true},
		{"no bound", `{"loop_branch":"a"}`, true},
		{"healthy", `{"loop_branch":"a","max_iterations":3}`, false},
	}
	for _, c := range cases {
		err := validateStepConfigForKind(flowStep{ID: "l", Name: "L", Kind: "loop_decision", Config: c.cfg})
		if c.bad && err == nil {
			t.Errorf("%s: expected a refusal (the reconciler fails this config)", c.name)
		}
		if !c.bad && err != nil {
			t.Errorf("%s: unexpected refusal: %v", c.name, err)
		}
	}
}

// A worker-backed approval with no worker ref fails the step at run time, so it is
// refused at the field. A human approval needs no ref.
func TestWorkerBackedApprovalNeedsARef(t *testing.T) {
	workerNoRef := flowStep{ID: "a", Kind: "approval", Config: `{"reviewer":"worker"}`}
	if err := validateStepConfigForKind(workerNoRef); err == nil {
		t.Error("a worker-backed approval with no ref must be refused — the reconciler fails it")
	}
	workerWithRef := flowStep{ID: "a", Kind: "approval", Ref: "w_x", Config: `{"reviewer":"worker"}`}
	if err := validateStepConfigForKind(workerWithRef); err != nil {
		t.Errorf("a worker-backed approval WITH a ref was refused: %v", err)
	}
	humanNoRef := flowStep{ID: "a", Kind: "approval", Config: `{"reviewer":"human"}`}
	if err := validateStepConfigForKind(humanNoRef); err != nil {
		t.Errorf("a human approval needs no worker ref: %v", err)
	}
	// Absent reviewer means the human path.
	if err := validateStepConfigForKind(flowStep{ID: "a", Kind: "approval"}); err != nil {
		t.Errorf("an approval with no reviewer is a human gate and must pass: %v", err)
	}
}

// The policy round-trips through the form, and an edit to an UNRELATED field must not
// disturb it — the same merge contract every other config key holds to.
func TestMissingDecisionRoundTripsAndSurvivesOtherEdits(t *testing.T) {
	const steps = `[{"id":"a","name":"A","kind":"loop_decision","config":"{\"loop_branch\":\"b\",\"max_iterations\":6,\"on_missing_decision\":\"success\"}","depends_on":["b"]},
	                {"id":"b","name":"Start","kind":"task","ref":"w_x","depends_on":[]}]`
	m, saved := cfgEditorModel(t, steps)

	// The form SHOWS the stored policy rather than the default.
	f := m.editStepForm(m.rawFlowSteps()[0])
	if got := f.Values["on_missing_decision"]; got != "success" {
		t.Fatalf("the form seeded on_missing_decision = %q, want the stored success", got)
	}

	// A rename alone leaves it alone.
	f.Set("name", "A (renamed)")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	var got map[string]any
	if err := json.Unmarshal([]byte(parseFlowSteps((*saved)[0])[0].Config), &got); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if got["on_missing_decision"] != "success" {
		t.Errorf("an unrelated edit disturbed on_missing_decision: %v", got)
	}

	// And setting it to reask is a real edit that lands.
	f2 := m.editStepForm(m.rawFlowSteps()[0])
	f2.Set("on_missing_decision", "reask")
	cmd2, err := f2.OnSubmit(f2.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd2)
	var got2 map[string]any
	last := parseFlowSteps((*saved)[len(*saved)-1])[0]
	if err := json.Unmarshal([]byte(last.Config), &got2); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if got2["on_missing_decision"] != "reask" {
		t.Errorf("on_missing_decision = %v, want reask", got2["on_missing_decision"])
	}
	// The loop's real keys survive the change.
	if got2["loop_branch"] != "b" {
		t.Errorf("loop_branch was lost: %v", got2)
	}
}

// A loop that carries NO policy reads as the engine default, because that is what will
// actually run — an empty row beside a gate with real behaviour would be a lie.
func TestMissingDecisionAbsentReadsAsTheEngineDefault(t *testing.T) {
	for _, tc := range []struct{ cfg, want string }{
		{"", "reask"},
		{"{}", "reask"},
		{`{"loop_branch":"a","max_iterations":3}`, "reask"},
		{`{"on_missing_decision":"fail"}`, "fail"},
		{"{broken", "reask"},
	} {
		if got := missingDecisionOf(tc.cfg); got != tc.want {
			t.Errorf("missingDecisionOf(%q) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

// max_reask is the other half of the missing-verdict question: the policy says what an
// absent verdict MEANS, this says how many times to ask for one first. It round-trips,
// it survives unrelated edits, and a blank/zero bound DELETES the key rather than
// writing a value the engine silently replaces with 3.
func TestMaxReaskRoundTripsAndDeletesWhenBlanked(t *testing.T) {
	const steps = `[{"id":"a","name":"A","kind":"loop_decision","config":"{\"loop_branch\":\"b\",\"max_iterations\":6,\"max_reask\":7,\"on_missing_decision\":\"fail\"}","depends_on":["b"]},
	                {"id":"b","name":"Start","kind":"task","ref":"w_x","depends_on":[]}]`
	m, saved := cfgEditorModel(t, steps)

	// The form shows the STORED bound, not the default.
	f := m.editStepForm(m.rawFlowSteps()[0])
	if got := f.Values["max_reask"]; got != "7" {
		t.Fatalf("the form seeded max_reask = %q, want the stored 7", got)
	}
	// An unrelated edit leaves it alone.
	f.Set("name", "A (renamed)")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	var got map[string]any
	if err := json.Unmarshal([]byte(parseFlowSteps((*saved)[0])[0].Config), &got); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if got["max_reask"].(float64) != 7 {
		t.Errorf("an unrelated edit disturbed max_reask: %v", got)
	}
	if got["on_missing_decision"] != "fail" {
		t.Errorf("the policy was disturbed: %v", got)
	}

	// Blanking it deletes the key, so the engine default applies honestly.
	f2 := m.editStepForm(m.rawFlowSteps()[0])
	f2.Set("max_reask", "")
	cmd2, err := f2.OnSubmit(f2.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd2)
	last := parseFlowSteps((*saved)[len(*saved)-1])[0]
	var got2 map[string]any
	if err := json.Unmarshal([]byte(last.Config), &got2); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if _, ok := got2["max_reask"]; ok {
		t.Errorf("a blank bound was written instead of deleted: %s", last.Config)
	}
	if got2["loop_branch"] != "b" {
		t.Errorf("the loop's real keys were lost: %v", got2)
	}
}

// The bound the form shows is the one the engine would apply — parseLoopDecisionConfig
// replaces anything <= 0 with 3, so an absent or zero value must read as 3.
func TestMaxReaskReadsAsTheEngineDefault(t *testing.T) {
	for _, tc := range []struct{ cfg, want string }{
		{"", "3"},
		{"{}", "3"},
		{`{"loop_branch":"a","max_iterations":3}`, "3"},
		{`{"max_reask":0}`, "3"},
		{`{"max_reask":-2}`, "3"},
		{`{"max_reask":9}`, "9"},
		{"{broken", "3"},
	} {
		if got := maxReaskOf(tc.cfg); got != tc.want {
			t.Errorf("maxReaskOf(%q) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

// A new TASK gets its own kind's keys and nothing else — no branch keys inherited
// from the form's other columns.
func TestAddStepWritesOnlyItsOwnKindsKeys(t *testing.T) {
	m, saved := cfgEditorModel(t, `[{"id":"a","name":"A","kind":"task","ref":"w_x","depends_on":[]}]`)
	f := m.addStepForm()
	f.Set("name", "QA")
	f.Set("kind", "task")
	f.Set("ref", "w_qa")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	after := parseFlowSteps((*saved)[0])
	if len(after) != 2 {
		t.Fatalf("saved %d steps, want 2", len(after))
	}
	added := after[1]
	var got map[string]any
	if err := json.Unmarshal([]byte(added.Config), &got); err != nil {
		t.Fatalf("new step config is not JSON: %v (%s)", err, added.Config)
	}
	if _, ok := got["loop_branch"]; ok {
		t.Errorf("a new task inherited a loop_branch: %s", added.Config)
	}
	if _, ok := got["success_branch"]; ok {
		t.Errorf("a new task inherited a success_branch: %s", added.Config)
	}
	rec, ok := got["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("a new task must carry the recovery policy its fields set: %s", added.Config)
	}
	if rec["strategy"] != "retry" {
		t.Errorf("recovery.strategy = %v, want the built-in retry", rec["strategy"])
	}
}
