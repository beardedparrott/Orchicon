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
	m.rpcSetWorkerModel = func(_ context.Context, workerID, ref string) error {
		*writes = append(*writes, workerID+"="+ref)
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

// The picker opens seeded from the worker's ACTIVE model_ref, which the workers
// list already carries — including a slashed model remainder, verbatim.
func TestWorkerModelPickerOpensSeededFromTheActiveRef(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.workerMu.Lock()
	m.workerModel["w-1"] = "orchicon/commandcode/deepseek/deepseek-v4-flash"
	m.workerMu.Unlock()

	cmd := m.beginSetModel("w-1")
	if m.modelPicker == nil {
		t.Fatal("beginSetModel must open the picker")
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

// The three-step walk WRITES the chosen ref for that worker (BulkUpdateWorkerModel
// with the single id), through the one mutation executor.
func TestWorkerModelPickerCascadeWritesTheChosenRef(t *testing.T) {
	m, loads, writes := newPickerExec(t)
	cmd := m.beginSetModel("w-1")

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
	if len(*writes) != 1 || (*writes)[0] != "w-1=orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("writes = %v, want the chosen ref for w-1", *writes)
	}
}

// esc backs out without writing.
func TestWorkerModelPickerEscWritesNothing(t *testing.T) {
	m, _, writes := newPickerExec(t)
	m.beginSetModel("w-1")

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
	m.beginSetModel("w-1")

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
func TestWorkerModelKeyIsGuarded(t *testing.T) {
	m, _, _ := newPickerExec(t)

	// Off the workers pane.
	m.SelectSource(srcExecutions)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")
	if _, handled := m.handleActionKey(keySetModel); !handled {
		t.Fatal("m must be handled (with a refusal) off the workers pane")
	}
	if m.modelPicker != nil {
		t.Fatal("no picker off the workers pane")
	}
	if !strings.Contains(m.notice, "Workers pane") {
		t.Fatalf("notice = %q, want a refusal naming the Workers pane", m.notice)
	}

	// On the workers pane with no selection.
	m.SelectSource(srcWorkers)
	m.notice = ""
	if _, handled := m.handleActionKey(keySetModel); !handled {
		t.Fatal("m must be handled on the workers pane")
	}
	if m.modelPicker != nil {
		t.Fatal("no picker without a selected worker")
	}
	if !strings.Contains(m.notice, "select a worker") {
		t.Fatalf("notice = %q, want a refusal to select a worker", m.notice)
	}
}

// With a worker selected, "m" opens the picker for THAT worker.
func TestWorkerModelKeyOpensThePickerForTheSelectedWorker(t *testing.T) {
	m, _, writes := newPickerExec(t)
	m.LoadItems(srcWorkers, []kit2.Item{{ID: "w-7", Title: "sweeper", Meta: "published v2"}}, "")
	m.SelectSource(srcWorkers)

	if _, handled := m.handleActionKey(keySetModel); !handled {
		t.Fatal("m must be handled")
	}
	if m.modelPicker == nil {
		t.Fatal("m must open the model picker for the selected worker")
	}
	if m.modelPickerWorker != "w-7" {
		t.Fatalf("picker target = %q, want w-7", m.modelPickerWorker)
	}
	if len(*writes) != 0 {
		t.Fatal("opening the picker must not write anything")
	}
	// The pane advertises the gesture. (The hint now also carries the Item 6
	// CRUD chords, so this asserts the MODEL gesture specifically rather than the
	// whole line.)
	if h := m.HintLine(); !strings.Contains(h, "m: set model") {
		t.Fatalf("the workers hint must advertise the set-model gesture, got %q", h)
	}
	// The row action is offered for the selection.
	found := false
	for _, a := range m.actionsForSelection() {
		if a.Key == keySetModel {
			found = true
		}
	}
	if !found {
		t.Fatal("the workers pane must offer the set-model action")
	}
}

// A load landing after the picker closed is dropped.
func TestWorkerModelPickerIgnoresALateLoad(t *testing.T) {
	m, _, _ := newPickerExec(t)
	m.beginSetModel("w-1")
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
