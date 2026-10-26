package work

// item_execution_mode_test.go — the work-item forms carry the EXECUTION-MODE OVERRIDE.
//
// The proto has three different spellings of the same idea and the differences are the whole trap
// (proto/orchicon/api/v1/work_item_service.proto):
//
//   - CreateWorkItemRequest.execution_mode is NON-optional: EXECUTION_MODE_UNSPECIFIED (the default)
//     IS inherit, so an item created from the TUI sends UNSPECIFIED for "inherit" and the enum for a
//     concrete choice.
//   - UpdateWorkItemRequest.execution_mode is `optional`: UNSET means UNCHANGED, and there is a
//     SEPARATE bool clear_execution_mode which the proto names as the ONLY way to remove an override.
//     Sending only the enum makes an override permanent; sending only the clear wipes a legitimate
//     new value on every save. CLEAR WINS when both are present, so the form sends exactly one.
//   - WorkItem.execution_mode is the OVERRIDE (unset = inherit) while effective_execution_mode is the
//     RESOLVED answer. The edit form is seeded from the first; the detail pane renders the second.
//
// These tests drive the real create/edit paths (the screen's own keys, forms and submit handler) and
// assert on the RECORDED requests AND on the fake plane's stored state, because a request can be right
// while the fake — and therefore the round-trip the operator sees — is wrong.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// modeValueOf reads one option's label out of a select spec.
func modeLabelOf(t *testing.T, f *kit2.Form, value string) string {
	t.Helper()
	spec := f.Spec("execution_mode")
	if spec == nil {
		t.Fatal("the form has no execution_mode field")
	}
	for _, o := range spec.Options {
		if o.Value == value {
			return o.Label
		}
	}
	t.Fatalf("execution_mode has no %q option", value)
	return ""
}

// modeValues lists the select's option values in order.
func modeValues(f *kit2.Form) []string {
	var out []string
	for _, o := range f.Spec("execution_mode").Options {
		out = append(out, o.Value)
	}
	return out
}

// openCreateFormP seeds two projects with DIFFERENT modes and opens the create form on `n`. The
// ordered pair matters: the first project is what the form opens on, the second is what a project
// change must be able to reach.
func openCreateFormP(t *testing.T) (*Model, *kit2.Form, *fakePlane) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Local project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	p.seedProject("proj-2", "Runtime project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	return m, f, p
}

// AC1 — THREE options, inherit first, and the inherit label states the project mode it inherits.
func TestCreateFormOffersThreeWayExecutionMode(t *testing.T) {
	_, f, _ := openCreateFormP(t)

	if got, want := modeValues(f), []string{"inherit", "local", "runtime"}; len(got) != len(want) {
		t.Fatalf("execution_mode options = %v, want exactly %v — an item may INHERIT, which the "+
			"project's two-way list cannot express", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("execution_mode options = %v, want %v (inherit first)", got, want)
			}
		}
	}

	// The form opens on INHERIT — a new item carries no override.
	if got := f.Values["execution_mode"]; got != itemExecutionModeInherit {
		t.Fatalf("the create form opened on %q, want %q (no override)", got, itemExecutionModeInherit)
	}
	// ...and the label says WHAT it would inherit: the first project is local.
	if got, want := modeLabelOf(t, f, itemExecutionModeInherit), "inherit from project (local)"; got != want {
		t.Fatalf("inherit label = %q, want %q — the operator must be able to see the answer, not "+
			"just the word \"inherit\"", got, want)
	}

	// Changing the project re-reads the inherited mode: proj-2 is runtime.
	f.Set("project", "proj-2")
	if got, want := modeLabelOf(t, f, itemExecutionModeInherit), "inherit from project (runtime)"; got != want {
		t.Fatalf("inherit label after switching project = %q, want %q", got, want)
	}
	// The VALUE is untouched by the label refresh: it is still inherit.
	if got := f.Values["execution_mode"]; got != itemExecutionModeInherit {
		t.Fatalf("switching project changed the execution_mode value to %q", got)
	}
}

// AC6 — on CREATE: inherit sends UNSPECIFIED, a concrete choice sends that enum (asserted on the
// captured CreateWorkItemRequest AND on what the plane stored).
func TestCreateSendsUnspecifiedForInheritAndTheEnumForAChoice(t *testing.T) {
	// (a) untouched → inherit → UNSPECIFIED, and the item carries NO override.
	m, f, p := openCreateFormP(t)
	f.Set("title", "Inheriting item")
	run(t, m, submit(t, m, "auto_start"))
	if len(p.created) != 1 {
		t.Fatalf("expected one create, got %d (SubmitErr=%q)", len(p.created), f.SubmitErr)
	}
	if got := p.created[0].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED {
		t.Fatalf("inherit on create sent %v, want EXECUTION_MODE_UNSPECIFIED (the create field is "+
			"non-optional and UNSPECIFIED IS inherit)", got)
	}
	var stored *apiv1.WorkItem
	for _, w := range p.items {
		if w.GetTitle() == "Inheriting item" {
			stored = w
		}
	}
	if stored == nil {
		t.Fatal("the created item is not in the fake plane")
	}
	if stored.GetExecutionMode() != apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED {
		t.Fatalf("the created item stored an override (%v) for an inherit choice",
			stored.GetExecutionMode())
	}

	// (b) an explicit choice → that enum, stored as the override.
	m2, f2, p2 := openCreateFormP(t)
	f2.Set("title", "Runtime item")
	f2.Set("execution_mode", "runtime")
	run(t, m2, submit(t, m2, "auto_start"))
	if len(p2.created) != 1 {
		t.Fatalf("expected one create, got %d (SubmitErr=%q)", len(p2.created), f2.SubmitErr)
	}
	if got := p2.created[0].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("choosing runtime on create sent %v, want EXECUTION_MODE_RUNTIME", got)
	}
	for _, w := range p2.items {
		if w.GetTitle() == "Runtime item" {
			if got := w.GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
				t.Fatalf("the created item stored %v, want the runtime override", got)
			}
		}
	}

	// (c) local is a real, distinguishable choice — not a synonym for inherit.
	m3, f3, p3 := openCreateFormP(t)
	f3.Set("title", "Local item")
	f3.Set("execution_mode", "local")
	run(t, m3, submit(t, m3, "auto_start"))
	if len(p3.created) != 1 {
		t.Fatalf("expected one create, got %d (SubmitErr=%q)", len(p3.created), f3.SubmitErr)
	}
	if got := p3.created[0].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("choosing local on create sent %v, want EXECUTION_MODE_LOCAL", got)
	}
}

// openItemEditorWithMode seeds a RUNTIME project holding one item whose OVERRIDE is `override` (nil =
// none) and opens that item's detail editor with `e`.
func openItemEditorWithMode(t *testing.T, override *apiv1.ExecutionMode) (*Model, *kit2.Form, *fakePlane) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Runtime project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		ExecutionMode: override,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	return m, f, p
}

// AC2 — the edit form PREFILLS from GetExecutionMode: an override opens on its value, no override
// opens on inherit, and it is NEVER blank (the kit2 Initial != "" rule).
func TestEditFormSeedsTheOverrideOrInherit(t *testing.T) {
	runtimeOverride := apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	m, f, _ := openItemEditorWithMode(t, &runtimeOverride)
	if got := f.Values["execution_mode"]; got != "runtime" {
		t.Fatalf("an item with a runtime override opened on %q, want runtime — an unseeded field "+
			"would open blank and submit blank", got)
	}
	// The default surface is a three-way control here too.
	if got := modeValues(f); len(got) != 3 {
		t.Fatalf("the edit form's execution_mode has %v, want inherit/local/runtime", got)
	}
	// The label names the project's mode (this project is runtime).
	if got, want := modeLabelOf(t, f, itemExecutionModeInherit), "inherit from project (runtime)"; got != want {
		t.Fatalf("inherit label = %q, want %q", got, want)
	}
	_ = m

	localOverride := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	_, f2, _ := openItemEditorWithMode(t, &localOverride)
	if got := f2.Values["execution_mode"]; got != "local" {
		t.Fatalf("an item with a local override opened on %q, want local", got)
	}

	_, f3, _ := openItemEditorWithMode(t, nil)
	if got := f3.Values["execution_mode"]; got != itemExecutionModeInherit {
		t.Fatalf("an item with NO override opened on %q, want %q — never blank", got, itemExecutionModeInherit)
	}
}

// AC3 — an UNTOUCHED edit changes nothing: the item keeps its runtime override and the request
// carries no clear. This is the "forms reset what you did not touch" failure class the file already
// documents for the reference pickers (workitems.go).
func TestUntouchedEditKeepsTheOverride(t *testing.T) {
	runtimeOverride := apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	m, f, p := openItemEditorWithMode(t, &runtimeOverride)

	run(t, m, submit(t, m, "auto_start"))
	if len(p.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p.updated), f.SubmitErr)
	}
	req := p.updated[0]
	if req.GetClearExecutionMode() {
		t.Fatal("an untouched execution mode must NOT send clear_execution_mode — that would strip " +
			"the override the operator never touched")
	}
	if req.ExecutionMode == nil {
		t.Fatal("an untouched override must round-trip as a VALUE")
	}
	if got := req.GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("round-tripped %v, want EXECUTION_MODE_RUNTIME", got)
	}
	// The stored state agrees: the override survived the save.
	if got := p.items["wi-1"].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("the stored override after an untouched save is %v, want runtime", got)
	}
}

// AC4 — the override can be SET from the edit form.
func TestEditCanSetTheOverride(t *testing.T) {
	m, f, p := openItemEditorWithMode(t, nil) // inheriting the project's runtime mode
	f.Set("execution_mode", "local")
	run(t, m, submit(t, m, "auto_start"))
	if len(p.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p.updated), f.SubmitErr)
	}
	req := p.updated[0]
	if req.GetClearExecutionMode() {
		t.Fatal("a concrete choice must not also clear")
	}
	if got := req.GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("choosing local sent %v, want EXECUTION_MODE_LOCAL", got)
	}
	if got := p.items["wi-1"].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("the override was not stored: %v", got)
	}
}

// AC5 — the override can be REMOVED: inherit sends clear_execution_mode, contradicted by nothing.
func TestEditChoosingInheritClearsTheOverride(t *testing.T) {
	runtimeOverride := apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	m, f, p := openItemEditorWithMode(t, &runtimeOverride)

	f.Set("execution_mode", itemExecutionModeInherit)
	run(t, m, submit(t, m, "auto_start"))
	if len(p.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p.updated), f.SubmitErr)
	}
	req := p.updated[0]
	if !req.GetClearExecutionMode() {
		t.Fatal("choosing inherit must send clear_execution_mode — it is the ONLY way to remove an " +
			"override, because an absent execution_mode means unchanged")
	}
	if req.ExecutionMode != nil {
		t.Fatalf("a clearing request must not also carry a stale enum (%v) — CLEAR WINS, and a "+
			"request that says two things at once is one whose outcome the caller had to guess",
			req.GetExecutionMode())
	}
	// Round trip: the stored override is GONE, and the item now resolves to its project's mode.
	if got := p.items["wi-1"].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED {
		t.Fatalf("the stored override after choosing inherit is %v, want unset", got)
	}
	if got := p.items["wi-1"].GetEffectiveExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("after clearing, the item resolves to %v, want the project's runtime", got)
	}
}

// AC8 — the DETAIL pane renders the EFFECTIVE mode, so an inheriting item shows what it resolves to.
func TestDetailPaneShowsTheEffectiveMode(t *testing.T) {
	// (a) an INHERITING item shows the project's mode, not a blank.
	p := newPlane()
	p.seedProject("proj-1", "Runtime project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Inheriting", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-1"))
	if got, want := fieldValue(t, m, "execution mode"), "runtime (inherited from the project)"; got != want {
		t.Fatalf("execution mode field = %q, want %q", got, want)
	}

	// (b) an OVERRIDDEN item names both halves: the effective mode IS the override.
	localOverride := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	p2 := newPlane()
	p2.seedProject("proj-1", "Runtime project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	p2.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Overridden", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		ExecutionMode: &localOverride,
	})
	m2 := newModel(t, p2)
	m2.SelectSource(srcWorkItems)
	load(t, m2, srcWorkItems)
	run(t, m2, m2.Base.RequestDetail(srcWorkItems, "wi-1"))
	if got, want := fieldValue(t, m2, "execution mode"), "local (item override: local)"; got != want {
		t.Fatalf("execution mode field = %q, want %q — the item opted OUT of its runtime project",
			got, want)
	}
}

// AC11 — THE OUTERMOST PROOF: the control is reachable and works through the surface an operator
// actually drives, not merely as a field spec. Everything below goes through the screen's own keys
// (`n` create, `e` edit, `space` to cycle a select, `ctrl+s` to submit) and is asserted on the
// RENDERED dialog (the same `m.View()` the TUI paints into the terminal) as well as on the request
// the plane received.
func TestOperatorDrivesExecutionModeFromTheWorkTab(t *testing.T) {
	// THE SCREEN REGION the operator is looking at, so a failure prints what was actually painted
	// instead of "want string, got string".
	modeLines := func(view string) string {
		var out []string
		for _, ln := range strings.Split(view, "\n") {
			if strings.Contains(strings.ToLower(ln), "execution mode") ||
				strings.Contains(ln, "inherit from project") ||
				strings.Contains(ln, "item override:") {
				out = append(out, ln)
			}
		}
		return strings.Join(out, "\n")
	}

	// ---- CREATE: `n` on Work Items → the field is on screen → cycle it → submit ----
	p := newPlane()
	p.seedProject("proj-1", "Local project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("`n` must open the create form")
	}
	if !f.FocusName("execution_mode") {
		t.Fatal("the create form has no execution_mode field to focus")
	}
	form := ansi.Strip(m.View())
	if !strings.Contains(form, "Execution mode") {
		t.Fatalf("the create form never paints the field:\n%s", modeLines(form))
	}
	if !strings.Contains(form, "inherit from project (local)") {
		t.Fatalf("the painted inherit option does not name the mode it inherits:\n%s", modeLines(form))
	}
	t.Logf("CREATE FORM, painted (execution-mode region):\n%s", modeLines(form))

	// Two spaces walk inherit -> local -> runtime: the operator's own gesture on a KSelect.
	f.Set("title", "Runtime item")
	press(t, m, "space")
	press(t, m, "space")
	after := ansi.Strip(m.View())
	if !strings.Contains(after, "runtime") {
		t.Fatalf("after cycling the select the painted value is not runtime:\n%s", modeLines(after))
	}
	t.Logf("CREATE FORM after the operator cycles to runtime:\n%s", modeLines(after))

	run(t, m, submit(t, m, "auto_start"))
	if len(p.created) != 1 {
		t.Fatalf("expected one create, got %d (SubmitErr=%q)", len(p.created), f.SubmitErr)
	}
	t.Logf("CREATE REQUEST: title=%q execution_mode=%v",
		p.created[0].GetTitle(), p.created[0].GetExecutionMode())
	if got := p.created[0].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("the operator chose runtime but the request carried %v", got)
	}

	// ---- DETAIL: the item's pane names the mode, and where it came from ----
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-new-1"))
	detail := ansi.Strip(m.View())
	if !strings.Contains(strings.ToLower(detail), "execution mode") {
		t.Fatalf("the detail pane has no execution-mode row:\n%s", detail)
	}
	t.Logf("DETAIL PANE (execution-mode region):\n%s", modeLines(detail))

	// ---- EDIT: `e` on an item that INHERITS → the field opens on inherit, and choosing
	//      runtime stores the override ----
	p2 := newPlane()
	p2.seedProject("proj-1", "Local project").ExecutionMode = apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	p2.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Inheriting item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m2 := newModel(t, p2)
	m2.SelectSource(srcWorkItems)
	load(t, m2, srcWorkItems)
	run(t, m2, press(t, m2, "e"))
	f2 := m2.ActiveForm()
	if f2 == nil {
		t.Fatal("`e` must open the edit form")
	}
	if !f2.FocusName("execution_mode") {
		t.Fatal("the edit form has no execution_mode field to focus")
	}
	if got := f2.Values["execution_mode"]; got != itemExecutionModeInherit {
		t.Fatalf("an inheriting item opened on %q, want inherit", got)
	}
	edit := ansi.Strip(m2.View())
	if !strings.Contains(edit, "inherit from project (local)") {
		t.Fatalf("the edit form does not paint the inherit option:\n%s", modeLines(edit))
	}
	t.Logf("EDIT FORM, painted (execution-mode region):\n%s", modeLines(edit))

	press(t, m2, "space") // inherit -> local
	run(t, m2, submit(t, m2, "auto_start"))
	if len(p2.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p2.updated), f2.SubmitErr)
	}
	t.Logf("EDIT REQUEST: execution_mode=%v clear_execution_mode=%t",
		p2.updated[0].GetExecutionMode(), p2.updated[0].GetClearExecutionMode())
	if got := p2.updated[0].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("the operator chose local but the update carried %v", got)
	}
	if got := p2.items["wi-1"].GetExecutionMode(); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("the plane stored %v, want the local override", got)
	}
}
