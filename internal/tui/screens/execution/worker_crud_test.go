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
	if _, handled := m.handleActionKey(keyNewWorker); !handled || m.form == nil {
		t.Fatal("n must open the create form")
	}
	if len(*calls) != 0 {
		t.Fatalf("create must not load a worker, got %v", *calls)
	}
	m.form = nil

	// The row operations refuse with the reason when nothing is selected.
	for _, k := range []string{keyEditWorker, keyEditVersion, keyPublish, keySetActive} {
		m.notice = ""
		if _, handled := m.handleActionKey(k); !handled {
			t.Fatalf("chord %q must be handled in the Workers pane", k)
		}
		if m.form != nil {
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
	if m2.form == nil {
		t.Fatal("publish with a draft must open the form")
	}
	if !strings.Contains(m2.form.Title, "Publish v2") {
		t.Fatalf("title = %q, want it to name the version being published", m2.form.Title)
	}
	cmd, _ := m2.form.OnSubmit(m2.form.Values, nil)
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
	if m.form == nil {
		t.Fatal("set-active must open a form when a published version exists")
	}
	// The select lists the published versions only — never the draft.
	var opts []string
	for _, s := range m.form.Specs {
		if s.Name == "version" {
			for _, o := range s.Options {
				opts = append(opts, o.Value)
			}
		}
	}
	if len(opts) != 2 || opts[0] != "1" || opts[1] != "3" {
		t.Fatalf("options = %v, want the two published versions [1 3]", opts)
	}
	cmd, err := m.form.OnSubmit(m.form.Values, nil)
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
	if m2.form != nil {
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
