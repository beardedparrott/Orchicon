package execution

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// newPickerExec builds an Execution screen whose worker-model loads and write are
// faked, so the whole adapter → provider → model cascade and the write are
// deterministic without a plane.
func newPickerExec(t *testing.T) (*Model, *[]string, *[]string) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	loads, writes := &[]string{}, &[]string{}
	m.rpcModelKinds = func(context.Context) ([]string, []string, error) {
		*loads = append(*loads, "kinds")
		// The Dispatcher reports opencode FIRST; the picker must still default
		// to the preferred native kind.
		return []string{"opencode", "orchicon"}, []string{"orchicon"}, nil
	}
	m.rpcModelProviders = func(_ context.Context, kind string) ([]kit2.PickerOption, error) {
		*loads = append(*loads, "providers:"+kind)
		return []kit2.PickerOption{{Value: "anthropic", Label: "anthropic"}}, nil
	}
	m.rpcModelModels = func(_ context.Context, kind, provider string) ([]kit2.PickerOption, bool, error) {
		*loads = append(*loads, "models:"+kind+"/"+provider)
		return []kit2.PickerOption{{Value: "claude-sonnet-4", Label: "claude-sonnet-4"}}, false, nil
	}
	// The write records the WHOLE batch in one entry, because that is what the screen sends: one
	// BulkUpdateWorkerModel call for the selection (a loop of single writes would make a ten-worker change
	// ten chances to half-apply). Recording `ids=ref` also lets a test see the ORDER and the COUNT.
	m.rpcSetWorkerModel = func(_ context.Context, workerIDs []string, ref string) error {
		*writes = append(*writes, strings.Join(workerIDs, ",")+"="+ref)
		return nil
	}
	return m, loads, writes
}

// runLoad runs a load cmd and routes its message back into the screen, returning
// the cascade's next hop.
func runLoad(t *testing.T, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a load cmd, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("load cmd produced no message")
	}
	_, next := m.Update(msg)
	return next
}

// runWrite executes a mutation cmd and asserts it succeeded.
func runWrite(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a write cmd, got nil")
	}
	res, ok := cmd().(mutate.Result)
	if !ok {
		t.Fatal("the write cmd must produce a mutate.Result")
	}
	if res.Err != nil {
		t.Fatalf("write failed: %v", res.Err)
	}
}

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// The picker opens seeded from the workers' SHARED ACTIVE model_ref — which the workers list already
// carries — including a slashed model remainder, verbatim.
func TestWorkerModelPickerOpensSeededFromTheActiveRef(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.workerMu.Lock()
	m.workerModel["w-1"] = "orchicon/commandcode/deepseek/deepseek-v4-flash"
	m.workerModel["w-2"] = "orchicon/commandcode/deepseek/deepseek-v4-flash"
	m.workerMu.Unlock()

	cmd := m.beginBulkSetModel([]string{"w-1", "w-2"})
	if m.modelPicker == nil {
		t.Fatal("beginBulkSetModel must open the picker")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open picker must claim the keyboard")
	}
	if got := m.modelPicker.Adapter(); got != "orchicon" {
		t.Fatalf("seeded adapter = %q, want orchicon", got)
	}
	if got := m.modelPicker.Provider(); got != "commandcode" {
		t.Fatalf("seeded provider = %q, want commandcode", got)
	}
	if got := m.modelPicker.Model(); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("seeded model = %q, want the slashed remainder verbatim", got)
	}
	if cmd == nil {
		t.Fatal("opening must dispatch the adapter load")
	}
}

// When the selected workers' models DIFFER there is nothing honest to seed with, so the picker starts at
// the preferred adapter rather than presenting one worker's ref as if it were everyone's.
func TestWorkerModelPickerDoesNotSeedFromOneOfMany(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.workerMu.Lock()
	m.workerModel["w-1"] = "orchicon/anthropic/claude-sonnet-4"
	m.workerModel["w-2"] = "orchicon/commandcode/deepseek/deepseek-v4-flash"
	m.workerMu.Unlock()

	m.beginBulkSetModel([]string{"w-1", "w-2"})
	if m.modelPicker == nil {
		t.Fatal("the picker must open")
	}
	if got := m.modelPicker.Model(); got != "" {
		t.Fatalf("a mixed selection must not seed a model, got %q", got)
	}
	if got := m.modelPicker.Provider(); got != "" {
		t.Fatalf("a mixed selection must not seed a provider, got %q", got)
	}
}

// The three-step walk WRITES the chosen ref for the WHOLE SELECTION in one call, through the one
// mutation executor.
func TestWorkerModelPickerCascadeWritesTheChosenRefForEveryMarkedWorker(t *testing.T) {
	m, loads, writes := newPickerExec(t)
	cmd := m.beginBulkSetModel([]string{"w-1", "w-2", "w-3"})

	// Adapter kinds → the provider tier is requested; the preferred kind wins.
	cmd = runLoad(t, m, cmd)
	if lastOf(*loads) != "kinds" {
		t.Fatalf("last load = %q, want kinds", lastOf(*loads))
	}
	if got := m.modelPicker.Adapter(); got != "orchicon" {
		t.Fatalf("fresh selection must default to the preferred adapter, got %q", got)
	}
	if cmd == nil {
		t.Fatal("the adapter tier must request its providers")
	}

	// Providers land; nothing loads until a provider is CHOSEN.
	cmd = runLoad(t, m, cmd)
	if lastOf(*loads) != "providers:orchicon" {
		t.Fatalf("last load = %q, want the provider tier", lastOf(*loads))
	}
	if cmd != nil {
		t.Fatal("nothing should load before the operator chooses a provider")
	}

	// Choose the adapter (advances), then the provider (loads its models).
	if _, next := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); next != nil {
		t.Fatal("choosing an adapter with its providers loaded must not re-fetch")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("choosing a provider must request its models")
	}
	cmd = runLoad(t, m, cmd)
	if lastOf(*loads) != "models:orchicon/anthropic" {
		t.Fatalf("last load = %q, want the model tier", lastOf(*loads))
	}
	if cmd != nil {
		t.Fatal("models landing must not trigger a further load")
	}

	// Choosing the model closes the picker AND issues the write.
	_, wcmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modelPicker != nil {
		t.Fatal("committing must close the picker")
	}
	runWrite(t, wcmd)
	if len(*writes) != 1 || (*writes)[0] != "w-1,w-2,w-3=orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("writes = %v, want ONE write covering all three marked workers", *writes)
	}
}

// esc backs out without writing.
func TestWorkerModelPickerEscWritesNothing(t *testing.T) {
	m, _, writes := newPickerExec(t)
	m.beginBulkSetModel([]string{"w-1", "w-2"})

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelPicker != nil {
		t.Fatal("esc must close the picker")
	}
	if len(*writes) != 0 {
		t.Fatalf("esc must not write, got %v", *writes)
	}
}

// While the picker is up it owns the keyboard: a screen chord must not fire
// underneath it.
func TestWorkerModelPickerOwnsTheKeyboard(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")
	m.beginBulkSetModel([]string{"w-1", "w-2"})

	// "c" is the cancel-execution chord; with the picker open it must be typed
	// into the search, not open a confirm dialog.
	press(t, m, "c")
	if m.DialogOpen() {
		t.Fatal("a screen chord fired while the picker owned the keyboard")
	}
	if m.modelPicker == nil {
		t.Fatal("the picker must stay open")
	}
}

// The "m" chord is guarded: it applies to the Workers pane and needs a selected
// worker — never a silent no-op.
func TestWorkerModelKeyIsGoneAndExplainsItself(t *testing.T) {
	m, _, _ := newPickerExec(t)

	// The chord is gone (the model is a form field now). It must still ANSWER,
	// rather than no-op, because a conversation or a muscle memory may send it.
	m.SelectSource(srcExecutions)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")
	if _, handled := m.handleActionKey(keySetModel); !handled {
		t.Fatal("m must still be handled, so it can explain itself")
	}
	if m.modelPicker != nil {
		t.Fatal("the removed chord must not open the picker")
	}
	if !strings.Contains(m.notice, "Edit form") {
		t.Fatalf("notice = %q, want it to name the form that holds the model", m.notice)
	}

	m.SelectSource(srcWorkers)
	m.notice = ""
	if _, handled := m.handleActionKey(keySetModel); !handled {
		t.Fatal("m must be handled on the workers pane too")
	}
	if m.modelPicker != nil {
		t.Fatal("the removed chord must not open the picker")
	}
	if !strings.Contains(m.notice, "version editor") {
		t.Fatalf("notice = %q, want it to name the version editor too", m.notice)
	}
}

// The model is set through the FORM now: choosing it fills the field, and the
// form's own submit performs the write.
func TestWorkerModelIsSetThroughTheFormField(t *testing.T) {
	m, _, writes := newPickerExec(t)
	m.SelectSource(srcWorkers)

	// The model field is seeded from the VERSION's own model_ref — not from the
	// workers-list cache. model_ref is VERSIONED state (ADR-0003) and the form edits
	// a specific version, so it has to show that version's stored ref; a cache that
	// holds the list's active ref would seed the form from a different version's
	// value (or with nothing at all, for a version the cache does not carry).
	w := &apiv1.Worker{Id: "w-7", Name: "sweeper"}
	versions := []*apiv1.WorkerVersion{{
		Id: "v1", Version: 1, ModelRef: "orchicon/anthropic/seed",
		Status: apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED,
	}}
	f, err := m.editWorkerForm(w, versions, nil)
	if err != nil {
		t.Fatalf("edit form: %v", err)
	}
	m.Base.BeginDetailEdit("Edit worker", f)
	if got := m.Base.DetailForm().Values["model_ref"]; got != "orchicon/anthropic/seed" {
		t.Fatalf("model field seeded with %q, want the version's stored ref", got)
	}
	if len(*writes) != 0 {
		t.Fatal("opening the form must not write anything")
	}

	// A picker opened FROM the form writes back into the field, not through a
	// competing RPC — the form's submit is the single writer.
	m.openFormModelPicker("model_ref", "orchicon/anthropic/seed")
	m.modelPicker.SetAdapters([]string{"orchicon"}, nil)
	m.modelPicker.SetProviders("orchicon", []kit2.PickerOption{{Value: "anthropic"}})
	m.modelPicker.SetModels("orchicon", "anthropic", []kit2.PickerOption{{Value: "claude-sonnet-4"}}, false)
	m.modelPicker.HandleKey(kmsg("enter"))
	m.finishModelPicker(m.modelPicker)
	if got := m.Base.DetailForm().Values["model_ref"]; got != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("model_ref = %q, want the chosen ref in the FIELD", got)
	}
	if len(*writes) != 0 {
		t.Fatalf("choosing must not write on its own — the form's submit does, got %v", *writes)
	}

	// The hint points at the forms that carry the field.
	if h := m.HintLine(); !strings.Contains(h, "edit") || !strings.Contains(h, "version") {
		t.Fatalf("the workers hint must point at the forms that carry the model, got %q", h)
	}
	// And the standalone row action is gone.
	for _, a := range m.actionsForSelection() {
		if a.Key == keySetModel {
			t.Fatal("the standalone set-model action must be gone — the model lives on the forms")
		}
	}
}

// A load landing after the picker closed is dropped.
func TestWorkerModelPickerIgnoresALateLoad(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.beginBulkSetModel([]string{"w-1", "w-2"})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	m.Update(modelKindsMsg{Kinds: []string{"orchicon"}})
	m.Update(modelProvidersMsg{Adapter: "orchicon"})
	m.Update(modelModelsMsg{Adapter: "orchicon", Provider: "anthropic"})
	if m.modelPicker != nil {
		t.Fatal("a late load must not resurrect the picker")
	}
}

// BulkUpdateWorkerModel reports its outcome PER WORKER, so a declined update must
// be raised as a failure — never read as an implicit success.
func TestWorkerModelOutcomeErrorTranslatesThePerWorkerResult(t *testing.T) {
	if err := workerModelOutcomeError("w-1", &apiv1.BulkUpdateWorkerModelResult{
		WorkerId: "w-1",
		Outcome: &apiv1.BulkUpdateWorkerModelResult_Updated{
			Updated: &apiv1.BulkUpdateWorkerModelUpdated{Version: 3, ModelRef: "orchicon/anthropic/x"},
		},
	}); err != nil {
		t.Fatalf("an updated worker must not be an error: %v", err)
	}

	err := workerModelOutcomeError("w-1", &apiv1.BulkUpdateWorkerModelResult{
		WorkerId: "w-1",
		Outcome: &apiv1.BulkUpdateWorkerModelResult_Skipped{
			Skipped: &apiv1.BulkUpdateWorkerModelSkipped{
				Reason: apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_DEPRECATED,
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("a skipped worker must be an error naming the reason, got %v", err)
	}

	err = workerModelOutcomeError("w-1", &apiv1.BulkUpdateWorkerModelResult{
		WorkerId: "w-1",
		Outcome: &apiv1.BulkUpdateWorkerModelResult_Error{
			Error: &apiv1.BulkUpdateWorkerModelError{Message: "boom"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a per-worker error must be surfaced, got %v", err)
	}

	if err := workerModelOutcomeError("w-1", &apiv1.BulkUpdateWorkerModelResult{WorkerId: "w-1"}); err == nil {
		t.Fatal("an empty outcome must not read as success")
	}
}

// Every skip reason reads as plain language, and the zero value is still honest.
func TestWorkerModelSkipReasonTexts(t *testing.T) {
	cases := []struct {
		reason apiv1.BulkUpdateWorkerModelSkipReason
		want   string
	}{
		{apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NOT_FOUND, "no longer exists"},
		{apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_DEPRECATED, "deprecated"},
		{apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_RETIRED, "retired"},
		{apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NO_PUBLISHED_VERSION, "no published version"},
		{apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_UNSPECIFIED, "declined"},
	}
	for _, c := range cases {
		if got := workerModelSkipReason(c.reason); !strings.Contains(got, c.want) {
			t.Errorf("workerModelSkipReason(%v) = %q, want it to contain %q", c.reason, got, c.want)
		}
	}
}
