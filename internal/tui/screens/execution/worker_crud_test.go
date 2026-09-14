package execution

// worker_crud_test.go — Item 6: the worker CRUD surface.
//
// These drive the REAL path: a write chord → the load thunk → the loaded
// message → the form the operation opened → its submit → the RPC thunk. That is
// the whole chain the operator exercises, and each hop has failed in this
// codebase before (a modal that could not close, a form that never opened), so
// asserting the parts in isolation would not have caught those.

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// crudExec builds an Execution screen with the worker CRUD loads and writes
// recorded, so the whole chain runs without a plane.
func crudExec(t *testing.T, worker *apiv1.Worker, versions []*apiv1.WorkerVersion) (*Model, *[]string, *[]string) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	calls := &[]string{}
	m.rpcGetWorker = func(context.Context, string) (*apiv1.Worker, error) {
		*calls = append(*calls, "get")
		if worker == nil {
			return nil, errors.New("not found")
		}
		return worker, nil
	}
	m.rpcListWorkerVersions = func(context.Context, string) ([]*apiv1.WorkerVersion, error) {
		*calls = append(*calls, "versions")
		return versions, nil
	}
	// One recorded sink per write so a test can assert WHICH write fired.
	writes := &[]string{}
	m.rpcCreateWorker = func(context.Context, *apiv1.CreateWorkerRequest) error {
		*writes = append(*writes, "create")
		return nil
	}
	m.rpcUpdateWorker = func(context.Context, *apiv1.UpdateWorkerRequest) error {
		*writes = append(*writes, "update")
		return nil
	}
	m.rpcDeleteWorker = func(context.Context, string) error { *writes = append(*writes, "delete"); return nil }
	m.rpcPublishWorkerVersion = func(context.Context, *apiv1.PublishWorkerVersionRequest) error {
		*writes = append(*writes, "publish")
		return nil
	}
	m.rpcDeprecateWorker = func(context.Context, string) error { *writes = append(*writes, "deprecate"); return nil }
	m.rpcSetActiveWorkerVersion = func(context.Context, string, int32) error {
		*writes = append(*writes, "setactive")
		return nil
	}
	m.rpcUpdateWorkerVersion = func(context.Context, *apiv1.UpdateWorkerVersionRequest) error {
		*writes = append(*writes, "updateversion")
		return nil
	}
	m.rpcCreateWorkerVersionWrite = func(context.Context, *apiv1.CreateWorkerVersionRequest) error {
		*writes = append(*writes, "createversion")
		return nil
	}
	return m, calls, writes
}

// detailForm is the INLINE editor's form (Item 3 moved the worker forms from a
// modal into the details pane), or nil.
func detailForm(m *Model) *kit2.Form { return m.Base.DetailForm() }

func draftV(id string, n int32) *apiv1.WorkerVersion {
	return &apiv1.WorkerVersion{Id: id, Version: n, Status: apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_DRAFT}
}

func pubV(id string, n int32, model string) *apiv1.WorkerVersion {
	return &apiv1.WorkerVersion{Id: id, Version: n, ModelRef: model, Status: apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED}
}

// The chords are scoped to the Workers pane. That matters because `p` is ALSO
// force-progress on a run: an unscoped switch would hijack it and refuse with a
// worker message.
func TestWorkerChordsAreScopedToTheWorkersPane(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m.Base.SelectSource("runs")
	for _, k := range []string{keyNewWorker, keyEditWorker, keyEditVersion, keyPublish, keySetActive} {
		if _, handled := m.handleActionKey(k); handled {
			t.Fatalf("chord %q was handled while the Runs pane was focused — it must fall through", k)
		}
	}
}

// Inside the Workers pane: `n` opens the create form (no load and no selection
// needed), while the operations that act on a ROW refuse by name when nothing is
// selected rather than opening a form against an empty id.
func TestWorkerChordsInsideTheWorkersPane(t *testing.T) {
	m, calls, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}

	// n needs no selection.
	if _, handled := m.handleActionKey(keyNewWorker); !handled || detailForm(m) == nil {
		t.Fatal("n must open the create form")
	}
	if len(*calls) != 0 {
		t.Fatalf("create must not load a worker, got %v", *calls)
	}
	m.Base.Update(kmsg("esc"))

	// The row operations refuse with the reason when nothing is selected.
	for _, k := range []string{keyEditWorker, keyEditVersion, keyPublish, keySetActive} {
		m.notice = ""
		if _, handled := m.handleActionKey(k); !handled {
			t.Fatalf("chord %q must be handled in the Workers pane", k)
		}
		if m.Base.DetailForm() != nil {
			t.Fatalf("chord %q must not open a form with no selection", k)
		}
		if !strings.Contains(m.notice, "select a worker") {
			t.Fatalf("chord %q notice = %q, want it to ask for a selection", k, m.notice)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("no selection must mean no load, got %v", *calls)
	}
}

// Editing the prompt goes through the version path: with a DRAFT present it
// UPDATES that draft; with none it CREATES one. Both are "edit the worker's
// version" to the operator, so both must work from the same chord.
func TestWorkerVersionEditUpdatesDraftOrCreatesOne(t *testing.T) {
	w := &apiv1.Worker{Id: "w1", Name: "writer"}

	// A draft exists → update it.
	m, _, writes := crudExec(t, w, []*apiv1.WorkerVersion{draftV("v2", 2), pubV("v1", 1, "orchicon/deepseek/deepseek-flash")})
	f, err := m.editWorkerVersionForm(w, []*apiv1.WorkerVersion{draftV("v2", 2), pubV("v1", 1, "")})
	if err != nil {
		t.Fatalf("version editor: %v", err)
	}
	if !strings.Contains(f.Title, "Edit version:") {
		t.Fatalf("title = %q, want the update path", f.Title)
	}
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(*writes) != 1 || (*writes)[0] != "updateversion" {
		t.Fatalf("writes = %v, want [updateversion] (a draft exists)", *writes)
	}

	// No draft → create one, and the title says so.
	m2, _, writes2 := crudExec(t, w, nil)
	f2, err := m2.editWorkerVersionForm(w, []*apiv1.WorkerVersion{pubV("v1", 1, "")})
	if err != nil {
		t.Fatalf("version editor: %v", err)
	}
	if !strings.Contains(f2.Title, "new draft") {
		t.Fatalf("title = %q, want it to name the create path", f2.Title)
	}
	cmd2, err := f2.OnSubmit(f2.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd2)
	if len(*writes2) != 1 || (*writes2)[0] != "createversion" {
		t.Fatalf("writes = %v, want [createversion] (no draft)", *writes2)
	}
}

// A worker with NO versions at all cannot be edited — refused with the reason
// rather than opening an empty form.
func TestWorkerVersionEditWithoutVersionsIsRefused(t *testing.T) {
	w := &apiv1.Worker{Id: "w1", Name: "writer"}
	m, _, _ := crudExec(t, w, nil)
	if _, err := m.editWorkerVersionForm(w, nil); err == nil {
		t.Fatal("a versionless worker must refuse the version editor")
	}
}

// publish requires a DRAFT: with none, the chord refuses and NAMES the reason
// instead of opening a form that would fail server-side.
func TestWorkerPublishRefusesWithoutADraft(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m.workerOp, m.workerOpID = opPublish, "w1"
	m.Update(workerDetailMsg{op: opPublish, workerID: "w1", worker: &apiv1.Worker{Id: "w1"},
		versions: []*apiv1.WorkerVersion{pubV("v1", 1, "")}})
	if m.form != nil {
		t.Fatal("publish with no draft must not open a form")
	}
	if !strings.Contains(m.notice, "DRAFT") {
		t.Fatalf("notice = %q, want it to name the missing draft", m.notice)
	}

	// With a draft it opens the publish form, naming the version it will ship.
	m2, _, writes := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m2.workerOp, m2.workerOpID = opPublish, "w1"
	m2.Update(workerDetailMsg{op: opPublish, workerID: "w1", worker: &apiv1.Worker{Id: "w1"},
		versions: []*apiv1.WorkerVersion{draftV("v2", 2)}})
	if detailForm(m2) == nil {
		t.Fatal("publish with a draft must open the form")
	}
	if !strings.Contains(detailForm(m2).Title, "Publish v2") {
		t.Fatalf("title = %q, want it to name the version being published", detailForm(m2).Title)
	}
	cmd, _ := detailForm(m2).OnSubmit(detailForm(m2).Values, nil)
	runWrite(t, cmd)
	if len(*writes) != 1 || (*writes)[0] != "publish" {
		t.Fatalf("writes = %v, want [publish]", *writes)
	}
}

// set-active offers only PUBLISHED versions, and refuses when there are none.
func TestWorkerSetActiveOffersPublishedVersionsOnly(t *testing.T) {
	m, _, writes := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m.workerOp, m.workerOpID = opSetActive, "w1"
	m.Update(workerDetailMsg{op: opSetActive, workerID: "w1", worker: &apiv1.Worker{Id: "w1"},
		versions: []*apiv1.WorkerVersion{pubV("v1", 1, "a/b/c"), pubV("v3", 3, "a/b/d"), draftV("v4", 4)}})
	if detailForm(m) == nil {
		t.Fatal("set-active must open a form when a published version exists")
	}
	// The select lists the published versions only — never the draft.
	var opts []string
	for _, s := range detailForm(m).Specs {
		if s.Name == "version" {
			for _, o := range s.Options {
				opts = append(opts, o.Value)
			}
		}
	}
	if len(opts) != 2 || opts[0] != "1" || opts[1] != "3" {
		t.Fatalf("options = %v, want the two published versions [1 3]", opts)
	}
	cmd, err := detailForm(m).OnSubmit(detailForm(m).Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(*writes) != 1 || (*writes)[0] != "setactive" {
		t.Fatalf("writes = %v, want [setactive]", *writes)
	}

	// No published version → refuse with the reason.
	m2, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m2.workerOp, m2.workerOpID = opSetActive, "w1"
	m2.Update(workerDetailMsg{op: opSetActive, workerID: "w1", worker: &apiv1.Worker{Id: "w1"},
		versions: []*apiv1.WorkerVersion{draftV("v1", 1)}})
	if m2.Base.DetailForm() != nil {
		t.Fatal("set-active with no published version must not open a form")
	}
	if !strings.Contains(m2.notice, "PUBLISHED") {
		t.Fatalf("notice = %q, want it to name the missing published version", m2.notice)
	}
}

// A load result for an operation the operator has moved on from is dropped — a
// stale async result must not pop a form over whatever they are doing now.
func TestWorkerLoadResultForAnAbandonedOpIsDropped(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	m.workerOp, m.workerOpID = opEditHeader, "w2"
	m.Update(workerDetailMsg{op: opEditHeader, workerID: "w1", worker: &apiv1.Worker{Id: "w1", Name: "late"}})
	if m.form != nil {
		t.Fatal("a result for a different worker must not open a form")
	}
}

// The keys avoid d/D on purpose: the shell's global routes consume those for the
// diff pane BEFORE the screen sees them, so a deprecate bound to D could never
// fire. This pins the choice so a future change cannot quietly reintroduce it.
func TestWorkerChordsAvoidTheGlobalDiffKeys(t *testing.T) {
	for _, k := range []string{keyNewWorker, keyEditWorker, keyEditVersion, keyPublish, keySetActive, keyDeprecate, keyDelete} {
		if k == "d" || k == "D" {
			t.Fatalf("worker chord %q collides with the global diff-pane toggle", k)
		}
	}
}

// Item 3: worker editing happens in the DETAILS PANE, never a modal, and the
// shell must be told the screen owns the keys while it is up.
func TestWorkerFormsOpenInlineInTheDetailsPaneNotAModal(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "writer"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	if _, handled := m.handleActionKey(keyNewWorker); !handled {
		t.Fatal("n must be handled")
	}
	if m.form != nil {
		t.Fatal("the worker form must NOT be a modal (Item 3)")
	}
	if !m.Base.EditingDetail() {
		t.Fatal("the worker form must open in the DETAILS PANE")
	}
	if !m.ClaimsKeys() {
		t.Fatal("while editing, the screen must claim the keys so chords cannot fire mid-edit")
	}

	// esc closes it and releases the keys.
	m.Base.Update(kmsg("esc"))
	if m.Base.EditingDetail() || m.ClaimsKeys() {
		t.Fatal("esc must close the inline editor and release the keys")
	}
}

// Item 2: the model is selectable in BOTH worker form modes. model_ref is a
// VERSION field (ADR-0003), so it belongs on the create form and the version
// editor — and on neither the header editor, whose RPC takes no model.
func TestWorkerFormsOfferTheModelPicker(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "writer"}, []*apiv1.WorkerVersion{draftV("v1", 1)})

	if f := m.createWorkerForm(); f == nil || !hasModelField(f) || f.OnOpenModelPicker == nil {
		t.Fatal("the create form must offer a model field wired to the picker")
	}
	w := &apiv1.Worker{Id: "w1", Name: "writer"}
	vf, err := m.editWorkerVersionForm(w, []*apiv1.WorkerVersion{draftV("v1", 1)})
	if err != nil {
		t.Fatalf("version editor: %v", err)
	}
	if !hasModelField(vf) || vf.OnOpenModelPicker == nil {
		t.Fatal("the version editor must offer a model field wired to the picker")
	}
	// The header editor HOLDS the model field too. It persists through
	// BulkUpdateWorkerModel rather than UpdateWorker (which carries no model) — the
	// documented edit-then-republish primitive, so one call serves a draft AND a
	// published worker. The operator: "the edit page of a worker should also have
	// the model selector ... It works on new versions, it should work on editing
	// current versions as well."
	if hf := m.editWorkerForm(w); !hasModelField(hf) || hf.OnOpenModelPicker == nil {
		t.Fatal("the header editor must offer a model field wired to the picker")
	}

	// Choosing a model writes it into the field (not through a competing write).
	// Seeding a full ref puts the picker on the MODEL tier, so one enter commits.
	m.Base.BeginDetailEdit("New worker", m.createWorkerForm())
	m.openFormModelPicker("model_ref", "orchicon/anthropic/seed")
	m.modelPicker.SetAdapters([]string{"orchicon"}, nil)
	m.modelPicker.SetProviders("orchicon", []kit2.PickerOption{{Value: "anthropic"}})
	m.modelPicker.SetModels("orchicon", "anthropic", []kit2.PickerOption{{Value: "claude-sonnet-4"}}, false)
	m.modelPicker.HandleKey(kmsg("enter"))
	if !m.modelPicker.Done() {
		t.Fatal("enter on the model tier must commit")
	}
	m.finishModelPicker(m.modelPicker)
	if got := m.Base.DetailForm().Values["model_ref"]; got != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("model_ref = %q, want the chosen ref written into the field", got)
	}
}

func hasModelField(f *kit2.Form) bool {
	if f == nil {
		return false
	}
	for _, s := range f.Specs {
		if s.Name == "model_ref" && s.Kind == kit2.KModel {
			return true
		}
	}
	return false
}

// The inline worker forms must own their keys: arrows move between FIELDS and esc
// cancels.
//
// "When editing a worker, I can't use the arrow keys to move between the different
// fields and it will not let me hit ESC to cancel out of editing the item" and
// "New worker form is the same way." The screen's write chords ran BEFORE the
// inline editor, and handleActionKey answers esc/up/down (it explains why a chord
// has nothing to run on), so it swallowed them before kit2.Base could move the
// cursor or close the editor.
func TestInlineWorkerFormOwnsArrowsAndEsc(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "writer"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	if _, handled := m.handleActionKey(keyNewWorker); !handled {
		t.Fatal("n must open the create form")
	}
	f := detailForm(m)
	if f == nil {
		t.Fatal("the create form must be open in the details pane")
	}
	start := f.Cursor

	// The screen must hand the key to the editor rather than to a write chord.
	_, cmd := m.Update(kmsg("down"))
	if cmd != nil {
		_ = cmd
	}
	if detailForm(m).Cursor == start {
		t.Fatalf("down did not move between fields (cursor stuck at %d) — a write chord swallowed it", start)
	}
	_, _ = m.Update(kmsg("up"))
	if detailForm(m).Cursor != start {
		t.Fatalf("up did not move back to field %d", start)
	}

	// esc cancels the edit outright.
	_, _ = m.Update(kmsg("esc"))
	if m.Base.EditingDetail() {
		t.Fatal("esc did not cancel the inline editor — a write chord swallowed it")
	}
}
